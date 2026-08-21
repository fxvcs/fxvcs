package gitops

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// FS presents a pinned component commit as a read-only fs.FS so the FiveM
// discovery and manifest-sandbox packages can scan it exactly as they scan a
// working tree. Release creation must re-scan the *pinned* tree, not whatever
// happens to be checked out, and this is the seam that makes those two code
// paths the same code path.
//
// Blob contents are read on demand and memoised; directory structure is
// derived once from `ls-tree -r`. Symlinks and gitlinks are not exposed: a
// manifest scanner that followed one could leave the commit it is supposed to
// be describing.
type FS struct {
	ctx     context.Context //nolint:containedctx // fs.FS has no context-carrying method
	tree    *Tree
	entries map[string]TreeEntry // normalized path -> blob
	dirs    map[string]map[string]bool
	cache   map[string][]byte
}

// FS builds the filesystem view of a pinned tree.
func (t *Tree) FS(ctx context.Context) (*FS, error) {
	entries, err := t.Entries(ctx)
	if err != nil {
		return nil, err
	}
	f := &FS{ctx: ctx, tree: t, entries: map[string]TreeEntry{}, dirs: map[string]map[string]bool{".": {}}, cache: map[string][]byte{}}
	for _, e := range entries {
		if !e.IsRegular() {
			continue
		}
		f.entries[e.Path] = e
		dir := path.Dir(e.Path)
		f.addChild(dir, path.Base(e.Path))
		for dir != "." {
			parent := path.Dir(dir)
			f.addChild(parent, path.Base(dir))
			dir = parent
		}
	}
	return f, nil
}

func (f *FS) addChild(dir, name string) {
	if f.dirs[dir] == nil {
		f.dirs[dir] = map[string]bool{}
	}
	f.dirs[dir][name] = true
}

// Open implements fs.FS.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if e, ok := f.entries[name]; ok {
		data, err := f.read(e)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &treeFile{info: fileInfo{name: path.Base(name), size: int64(len(data)), mode: modeOf(e)}, r: bytes.NewReader(data)}, nil
	}
	if _, ok := f.dirs[name]; ok {
		list, err := f.ReadDir(name)
		if err != nil {
			return nil, err
		}
		return &treeDir{info: fileInfo{name: path.Base(name), dir: true}, entries: list}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	children, ok := f.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	names := make([]string, 0, len(children))
	for c := range children {
		names = append(names, c)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, 0, len(names))
	for _, c := range names {
		full := c
		if name != "." {
			full = name + "/" + c
		}
		if e, isFile := f.entries[full]; isFile {
			out = append(out, fs.FileInfoToDirEntry(fileInfo{name: c, size: e.Size, mode: modeOf(e)}))
			continue
		}
		out = append(out, fs.FileInfoToDirEntry(fileInfo{name: c, dir: true}))
	}
	return out, nil
}

// Stat implements fs.StatFS.
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if e, ok := f.entries[name]; ok {
		return fileInfo{name: path.Base(name), size: e.Size, mode: modeOf(e)}, nil
	}
	if _, ok := f.dirs[name]; ok {
		return fileInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadFile implements fs.ReadFileFS.
func (f *FS) ReadFile(name string) ([]byte, error) {
	e, ok := f.entries[name]
	if !ok {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrNotExist}
	}
	return f.read(e)
}

func (f *FS) read(e TreeEntry) ([]byte, error) {
	if data, ok := f.cache[e.OID]; ok {
		return data, nil
	}
	data, err := f.tree.ReadBlob(f.ctx, e.OID)
	if err != nil {
		return nil, err
	}
	f.cache[e.OID] = data
	return data, nil
}

func modeOf(e TreeEntry) fs.FileMode {
	if e.Mode == "100755" {
		return 0o755
	}
	return 0o644
}

type fileInfo struct {
	name string
	size int64
	mode fs.FileMode
	dir  bool
}

func (i fileInfo) Name() string { return i.name }
func (i fileInfo) Size() int64  { return i.size }
func (i fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return i.mode
}
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }

type treeFile struct {
	info fileInfo
	r    *bytes.Reader
}

func (f *treeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *treeFile) Close() error               { return nil }

type treeDir struct {
	info    fileInfo
	entries []fs.DirEntry
	off     int
}

func (d *treeDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}
func (d *treeDir) Close() error { return nil }
func (d *treeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := d.entries[d.off:]
		d.off = len(d.entries)
		return out, nil
	}
	if d.off >= len(d.entries) {
		return nil, io.EOF
	}
	end := min(d.off+n, len(d.entries))
	out := d.entries[d.off:end]
	d.off = end
	return out, nil
}

// PathList returns every regular file path in the tree, sorted.
func (f *FS) PathList() []string {
	out := make([]string, 0, len(f.entries))
	for p := range f.entries {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Entry returns the tree entry for a path.
func (f *FS) Entry(p string) (TreeEntry, bool) {
	e, ok := f.entries[strings.TrimPrefix(p, "./")]
	return e, ok
}
