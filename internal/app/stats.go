package app

import (
	"context"
	"sort"

	"fxvcs.dev/fxvcs/internal/pointer"
)

// StorageStats is what content addressing is buying this repository, measured
// against the only honest baseline: storing every tracked file's bytes once
// per path, which is what a plain shared folder or an FTP upload does.
//
// It is deliberately a point-in-time measurement of one repository. Savings
// across repositories are not a thing this can report and not a thing FXVCS
// does: one repository is one storage domain, content is never deduplicated
// across access boundaries, and a number that pretended otherwise would be
// describing a system with different security properties.
type StorageStats struct {
	// Assets is every tracked path in the index; Contents is how many
	// distinct files they are.
	Assets   int
	Contents int

	// LogicalBytes is what the assets weigh laid out as ordinary files, one
	// copy per path. StoredBytes is what they actually occupy as objects.
	LogicalBytes int64
	StoredBytes  int64

	// The saving splits into two mechanisms, which are worth telling apart
	// because they scale differently: whole files that are byte-identical,
	// and pieces shared between files that merely resemble each other (the
	// same asset at two versions, or two variants of one texture).
	IdenticalFiles      int
	IdenticalFileBytes  int64
	FilesSharingParts   int
	SharedPartBytes     int64
	SharedObjects       int
	ChunkedAssets       int
	WholeAssets         int
	LargestSharedObject int64

	// Unresolved counts assets whose object manifest is not available
	// locally, so their pieces could not be examined. They are still counted
	// in Assets and LogicalBytes; the saving figures are a lower bound while
	// this is non-zero.
	Unresolved int
}

// SavedBytes is the difference between laying every path out as its own file
// and storing the objects behind them.
func (s StorageStats) SavedBytes() int64 { return s.LogicalBytes - s.StoredBytes }

// SavedFraction is the saving as a fraction of the logical size.
func (s StorageStats) SavedFraction() float64 {
	if s.LogicalBytes <= 0 {
		return 0
	}
	return float64(s.SavedBytes()) / float64(s.LogicalBytes)
}

// StorageStatsOptions configures the measurement.
type StorageStatsOptions struct {
	// Fetch allows resolving object manifests that are not cached locally,
	// which costs one request per unexamined asset. Off by default: a report
	// should not quietly download a repository's worth of metadata.
	Fetch bool
}

// StorageStats measures deduplication across the current index.
func (r *Repo) StorageStats(ctx context.Context, sel Selection, opts StorageStatsOptions) (StorageStats, error) {
	entries, _, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return StorageStats{}, err
	}
	var st StorageStats

	// Group paths by content. Identical files are the first mechanism, and
	// they are free to detect: the pointer already names the content.
	byContent := map[string][]IndexEntry{}
	var order []string
	for _, e := range entries {
		if !e.HasPointer {
			continue
		}
		st.Assets++
		st.LogicalBytes += e.Pointer.Size
		if _, seen := byContent[e.Pointer.OID]; !seen {
			order = append(order, e.Pointer.OID)
		}
		byContent[e.Pointer.OID] = append(byContent[e.Pointer.OID], e)
	}
	st.Contents = len(order)
	for _, oid := range order {
		paths := byContent[oid]
		if len(paths) > 1 {
			st.IdenticalFiles += len(paths) - 1
			st.IdenticalFileBytes += paths[0].Pointer.Size * int64(len(paths)-1)
		}
	}

	// The second mechanism needs the manifests: which objects each distinct
	// file is made of, and which of those objects more than one file uses.
	type objectUse struct {
		size  int64
		refs  int
		first string // the content that introduced this object
	}
	objects := map[string]*objectUse{}
	contentsWithSharedParts := map[string]bool{}
	for _, oid := range order {
		e := byContent[oid][0]
		if !opts.Fetch && !r.contentIsLocal(e.Pointer) {
			st.Unresolved++
			// Its bytes are still stored somewhere; assume no sharing rather
			// than inventing a saving.
			st.StoredBytes += e.Pointer.Size
			continue
		}
		c, err := r.Content(ctx, e.Pointer)
		if err != nil {
			st.Unresolved++
			st.StoredBytes += e.Pointer.Size
			continue
		}
		if c.Chunked() {
			st.ChunkedAssets++
		} else {
			st.WholeAssets++
		}
		for _, o := range c.Objects {
			use, ok := objects[o.Digest]
			if !ok {
				objects[o.Digest] = &objectUse{size: o.Size, refs: 1, first: oid}
				st.StoredBytes += o.Size
				continue
			}
			use.refs++
			if use.refs == 2 {
				st.SharedObjects++
				if o.Size > st.LargestSharedObject {
					st.LargestSharedObject = o.Size
				}
				// Sharing takes two: the content that introduced the object
				// is sharing it just as much as the one that found it there.
				contentsWithSharedParts[use.first] = true
			}
			contentsWithSharedParts[oid] = true
		}
	}
	// Attribute the saving. Identical files never reach the object layer at
	// all, so what is left after them is what sharing pieces between
	// different files bought.
	st.FilesSharingParts = len(contentsWithSharedParts)
	st.SharedPartBytes = st.SavedBytes() - st.IdenticalFileBytes
	if st.SharedPartBytes < 0 {
		st.SharedPartBytes = 0
	}
	return st, nil
}

// contentIsLocal reports whether a pointer's manifest can be read without a
// network request.
func (r *Repo) contentIsLocal(ptr pointer.Pointer) bool {
	if ptr.Encoding == pointer.EncodingWhole {
		return true // its manifest is derived, not stored
	}
	return r.Cache.Has(r.Domain, ptr.ManifestDigest)
}

// TopSharedObjects lists the most-reused objects, for a report that wants to
// name what the sharing is actually made of.
func (r *Repo) TopSharedObjects(ctx context.Context, sel Selection, limit int) ([]SharedObject, error) {
	entries, _, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, err
	}
	type use struct {
		size  int64
		paths map[string]bool
	}
	objects := map[string]*use{}
	// Resolve each distinct content once, but attribute its objects to every
	// path that uses it. Counting one path per content would hide the most
	// common kind of reuse there is: the same file vendored into a dozen
	// resources.
	resolved := map[string][]ContentObject{}
	for _, e := range entries {
		if !e.HasPointer {
			continue
		}
		objs, ok := resolved[e.Pointer.OID]
		if !ok {
			if !r.contentIsLocal(e.Pointer) {
				continue
			}
			c, err := r.Content(ctx, e.Pointer)
			if err != nil {
				resolved[e.Pointer.OID] = nil
				continue
			}
			objs = c.Objects
			resolved[e.Pointer.OID] = objs
		}
		for _, o := range objs {
			u, ok := objects[o.Digest]
			if !ok {
				u = &use{size: o.Size, paths: map[string]bool{}}
				objects[o.Digest] = u
			}
			u.paths[e.Path] = true
		}
	}
	var out []SharedObject
	for digest, u := range objects {
		if len(u.paths) < 2 {
			continue
		}
		paths := make([]string, 0, len(u.paths))
		for p := range u.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out = append(out, SharedObject{
			Digest: digest, Bytes: u.size, Users: len(u.paths),
			SavedBytes: u.size * int64(len(u.paths)-1), Paths: paths,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SavedBytes != out[j].SavedBytes {
			return out[i].SavedBytes > out[j].SavedBytes
		}
		return out[i].Digest < out[j].Digest
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SharedObject is one stored object more than one asset uses.
type SharedObject struct {
	Digest     string
	Bytes      int64
	Users      int
	SavedBytes int64
	Paths      []string
}
