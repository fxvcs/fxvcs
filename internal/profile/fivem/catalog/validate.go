package catalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// Problem is one validation finding.
type Problem struct {
	Path    string `json:"path"`  // catalog entry path ("" for catalog-wide)
	Field   string `json:"field"` // offending field
	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Field + ": " + p.Message
	}
	return p.Path + " " + p.Field + ": " + p.Message
}

// ValidationError aggregates every problem; callers see all findings at once.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return "catalog: not releasable: " + strings.Join(parts, "; ")
}

// ErrNotReleasable lets callers errors.Is a *ValidationError.
var ErrNotReleasable = errors.New("catalog: not releasable")

func (e *ValidationError) Is(target error) bool { return target == ErrNotReleasable }

// ValidateOptions extends the set of names visible to ordering references.
type ValidateOptions struct {
	// KnownNames are resource names supplied by other components, the runtime
	// catalog, or external resources. after/before may reference them.
	KnownNames []string
	// AllowPending leaves undecided entries to the caller.
	//
	// A component on its own must decide every resource before it can be
	// released — that is what `fxvcs resources validate` checks. Composition
	// is different: a coordination override or a release-only --enable is a
	// legitimate way to decide one, and the activation planner reports what is
	// still undecided after all of them have been applied. Reporting it twice,
	// once without knowing about the overrides, would tell an operator to fix
	// something they already fixed.
	AllowPending bool
}

// ValidateForRelease checks that every catalog entry can enter a Release:
// no pending or missing entries, valid and unique names/paths, resolvable
// after/before references, no ordering cycles, safe non-overlapping persistent
// paths, and a known mutableMode. It returns nil or a *ValidationError.
func ValidateForRelease(cat *domain.ResourceCatalog, opts ValidateOptions) error {
	var probs []Problem
	add := func(path, field, msg string) { probs = append(probs, Problem{Path: path, Field: field, Message: msg}) }
	if cat == nil {
		return &ValidationError{Problems: []Problem{{Field: "catalog", Message: "nil catalog"}}}
	}

	known := map[string]bool{}
	for _, n := range opts.KnownNames {
		known[n] = true
	}
	names := map[string]string{}
	paths := map[string]bool{}
	local := map[string]bool{} // names and provides declared by this catalog
	for _, r := range cat.Spec.Resources {
		if _, err := pathsafe.Normalize(r.Path); err != nil {
			add(r.Path, "path", err.Error())
		}
		if paths[r.Path] {
			add(r.Path, "path", "duplicate path")
		}
		paths[r.Path] = true
		if err := discovery.ValidateResourceName(r.Name); err != nil {
			add(r.Path, "name", err.Error())
		} else if prev, dup := names[r.Name]; dup {
			add(r.Path, "name", fmt.Sprintf("duplicate resource name %q (also %s)", r.Name, prev))
		} else {
			names[r.Name] = r.Path
		}
		local[r.Name] = true
		for _, p := range r.Provides {
			local[p] = true
		}
	}
	for _, r := range cat.Spec.Resources {
		switch r.Status {
		case StatusPresent:
		case StatusMissing:
			add(r.Path, "status", "missing (no resource directory); remove the entry or restore the resource")
		default:
			add(r.Path, "status", fmt.Sprintf("unknown status %q", r.Status))
		}
		switch r.Activation {
		case domain.ActivationEnabled, domain.ActivationDisabled:
		case domain.ActivationPending:
			if !opts.AllowPending {
				add(r.Path, "activation", "pending; set enabled or disabled")
			}
		default:
			add(r.Path, "activation", fmt.Sprintf("unknown activation %q", r.Activation))
		}
		switch r.MutableMode {
		case MutableImmutable, MutableCopy:
		default:
			add(r.Path, "mutableMode", fmt.Sprintf("must be %s or %s", MutableImmutable, MutableCopy))
		}
		for _, field := range []struct {
			name string
			refs []string
		}{{"after", r.After}, {"before", r.Before}} {
			for _, ref := range field.refs {
				if ref == r.Name {
					add(r.Path, field.name, "references itself")
					continue
				}
				if !local[ref] && !known[ref] {
					add(r.Path, field.name, fmt.Sprintf("unknown resource %q", ref))
				}
			}
		}
		var normalized []string
		declared := map[string]bool{}
		for _, pp := range r.PersistentPaths {
			n, err := pathsafe.Normalize(pp)
			if err != nil {
				add(r.Path, "persistentPaths", err.Error())
				continue
			}
			for _, prior := range normalized {
				if pathsafe.Overlaps(prior, n) {
					add(r.Path, "persistentPaths", fmt.Sprintf("%q overlaps %q", n, prior))
				}
			}
			normalized = append(normalized, n)
			declared[n] = true
			kind := r.PersistentPathKinds[n]
			if kind != "file" && kind != "directory" {
				add(r.Path, "persistentPathKinds", fmt.Sprintf("%q must declare kind file or directory", n))
			}
		}
		for pp := range r.PersistentPathKinds {
			n, err := pathsafe.Normalize(pp)
			if err != nil {
				add(r.Path, "persistentPathKinds", err.Error())
				continue
			}
			if !declared[n] {
				add(r.Path, "persistentPathKinds", fmt.Sprintf("%q is not listed in persistentPaths", n))
			}
		}
	}
	for _, cyc := range findCycles(cat) {
		add("", "ordering", "cycle: "+strings.Join(cyc, " -> "))
	}
	if len(probs) == 0 {
		return nil
	}
	sort.SliceStable(probs, func(i, j int) bool {
		if probs[i].Path != probs[j].Path {
			return probs[i].Path < probs[j].Path
		}
		return probs[i].Field < probs[j].Field
	})
	return &ValidationError{Problems: probs}
}

// findCycles builds the local ordering graph (edge u -> v means u must be
// ensured before v) from after, before, and dependencies that resolve to
// local names/provides, and returns every cycle found, deterministically.
func findCycles(cat *domain.ResourceCatalog) [][]string {
	byName := map[string]string{} // name or provide -> resource name
	for _, r := range cat.Spec.Resources {
		byName[r.Name] = r.Name
	}
	for _, r := range cat.Spec.Resources {
		for _, p := range r.Provides {
			if _, taken := byName[p]; !taken {
				byName[p] = r.Name
			}
		}
	}
	edges := map[string]map[string]bool{}
	addEdge := func(u, v string) {
		if u == v {
			return
		}
		if edges[u] == nil {
			edges[u] = map[string]bool{}
		}
		edges[u][v] = true
	}
	nodes := make([]string, 0, len(cat.Spec.Resources))
	for _, r := range cat.Spec.Resources {
		nodes = append(nodes, r.Name)
		for _, a := range r.After {
			if u, ok := byName[a]; ok {
				addEdge(u, r.Name)
			}
		}
		for _, b := range r.Before {
			if v, ok := byName[b]; ok {
				addEdge(r.Name, v)
			}
		}
		for _, d := range r.Dependencies {
			if u, ok := byName[d]; ok {
				addEdge(u, r.Name)
			}
		}
	}
	sort.Strings(nodes)
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycles [][]string
	var visit func(n string)
	visit = func(n string) {
		color[n] = gray
		stack = append(stack, n)
		next := make([]string, 0, len(edges[n]))
		for v := range edges[n] {
			next = append(next, v)
		}
		sort.Strings(next)
		for _, v := range next {
			switch color[v] {
			case white:
				visit(v)
			case gray:
				for i, s := range stack {
					if s == v {
						cyc := append(append([]string{}, stack[i:]...), v)
						cycles = append(cycles, cyc)
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	for _, n := range nodes {
		if color[n] == white {
			visit(n)
		}
	}
	return cycles
}
