package stack

import (
	"fmt"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
)

// The activation plan is the answer to "what does this release actually
// start?", and it must be answerable from the Release alone. Three sources
// contribute, in a fixed precedence:
//
//  1. the component's committed catalog (or the coordination runtime catalog
//     for runtime:/external: resources) — the default intent, owned by whoever
//     owns the code;
//  2. the coordination override file — composition-specific changes, keyed by
//     stable identity, limited to activation/after/before;
//  3. release-only overrides from `release create --enable/--disable`.
//
// Every entry records which one decided it. Nothing here resolves a conflict
// by preference or timestamp: a resource still `pending` after all three, or a
// cycle in the ordering constraints, fails the release.

// Override is one release-only activation choice.
type Override struct {
	// Selector is a resource name, a "<repositoryID>:<path>" component
	// identity, or a "runtime:"/"external:" resource ID.
	Selector string
	// Activation is enabled or disabled.
	Activation domain.Activation
}

// PlanInput is everything the planner needs.
type PlanInput struct {
	// Components are the pinned components, already validated.
	Components []Component
	// Runtime is the coordination-owned runtime/external catalog.
	Runtime *domain.RuntimeResourceCatalog
	// Overrides is the coordination override file.
	Overrides *domain.ResourceOverrides
	// ReleaseOverrides are release-only --enable/--disable choices.
	ReleaseOverrides []Override
}

// Plan is the resolved activation plan plus the runtime entries it needs.
type Plan struct {
	Resources        []domain.ReleaseResource
	RuntimeResources []domain.RuntimeResource
}

// EnabledNames returns the names the generated cfg would ensure, in order.
func (p Plan) EnabledNames() []string {
	out := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		if r.Activation == domain.ActivationEnabled {
			out = append(out, r.Name)
		}
	}
	return out
}

// componentKey is the stable identity of a component resource.
func componentKey(repositoryID, path string) string { return repositoryID + ":" + path }

// ResolvePlan builds the activation plan or returns every problem.
func ResolvePlan(in PlanInput) (Plan, []Problem) {
	var probs []Problem
	add := func(comp, field, msg string) {
		probs = append(probs, Problem{Component: comp, Field: field, Message: msg})
	}

	// --- collect entries in a deterministic base order --------------------
	// Runtime and external resources come first: on a real server the runtime
	// system resources are started before the content that depends on them,
	// and a stable base order is what makes ensure order reproducible when the
	// after/before constraints leave a choice.
	type entry struct {
		res       domain.ReleaseResource
		key       string // override identity
		provides  []string
		component string
	}
	var entries []entry
	byName := map[string]int{}

	if in.Runtime != nil {
		for _, rr := range in.Runtime.Spec.Resources {
			entries = append(entries, entry{
				res: domain.ReleaseResource{
					Source:           rr.Source,
					ResourceID:       rr.ID,
					Name:             rr.Name,
					Activation:       rr.Activation,
					ActivationSource: domain.ActivationSourceRuntime,
					After:            append([]string(nil), rr.After...),
					Before:           append([]string(nil), rr.Before...),
				},
				key: rr.ID,
			})
		}
	}
	comps := append([]Component(nil), in.Components...)
	SortComponents(comps)
	for _, c := range comps {
		if c.Catalog == nil {
			continue
		}
		res := append([]domain.CatalogResource(nil), c.Catalog.Spec.Resources...)
		sort.SliceStable(res, func(i, j int) bool { return res[i].Path < res[j].Path })
		for _, r := range res {
			if r.Status == domain.ResourceStatusMissing {
				add(c.Spec.RepositoryID, "resources", fmt.Sprintf("%s is marked missing in the pinned catalog; run fxvcs resources sync in the component and commit the result", r.Path))
				continue
			}
			entries = append(entries, entry{
				res: domain.ReleaseResource{
					Source:           domain.ResourceSourceComponent,
					RepositoryID:     c.Spec.RepositoryID,
					RepositoryName:   c.DisplayName(),
					Path:             r.Path,
					Name:             r.Name,
					Activation:       r.Activation,
					ActivationSource: domain.ActivationSourceComponent,
					After:            append([]string(nil), r.After...),
					Before:           append([]string(nil), r.Before...),
					Dependencies:     append([]string(nil), r.Dependencies...),
					Provides:         append([]string(nil), r.Provides...),
				},
				key:       componentKey(c.Spec.RepositoryID, r.Path),
				provides:  r.Provides,
				component: c.Spec.RepositoryID,
			})
		}
	}

	for i, e := range entries {
		if prev, dup := byName[e.res.Name]; dup {
			add(e.component, "name", fmt.Sprintf("resource name %q is declared twice (%s and %s); FiveM resource names are global",
				e.res.Name, describe(entries[prev].res), describe(e.res)))
			continue
		}
		byName[e.res.Name] = i
	}

	// --- apply coordination overrides -------------------------------------
	index := map[string]int{}
	for i, e := range entries {
		index[e.key] = i
	}
	if in.Overrides != nil {
		for _, ov := range in.Overrides.Spec.Resources {
			key := ov.ResourceID
			if key == "" {
				key = componentKey(ov.RepositoryID, ov.Path)
			}
			i, ok := index[key]
			if !ok {
				add(ov.RepositoryID, "overrides", fmt.Sprintf("override for %s matches no resource in this composition", key))
				continue
			}
			applyOverride(&entries[i].res, ov.Activation, ov.After, ov.Before, domain.ActivationSourceCoordination)
		}
	}

	// --- apply release-only overrides -------------------------------------
	for _, ro := range in.ReleaseOverrides {
		i, ok := index[ro.Selector]
		if !ok {
			if j, byname := byName[ro.Selector]; byname {
				i, ok = j, true
			}
		}
		if !ok {
			add("", "override", fmt.Sprintf("--%s %s matches no resource in this composition", activationFlag(ro.Activation), ro.Selector))
			continue
		}
		act := ro.Activation
		applyOverride(&entries[i].res, &act, nil, nil, domain.ActivationSourceRelease)
	}

	// --- everything must be decided ---------------------------------------
	for _, e := range entries {
		switch e.res.Activation {
		case domain.ActivationEnabled, domain.ActivationDisabled:
		default:
			add(e.component, "activation", fmt.Sprintf("%s is still pending; decide it in the component (fxvcs resources set %s --activation enabled|disabled), in the coordination overrides, or with --enable/--disable",
				describe(e.res), quoteArg(e.res.Path)))
		}
	}

	// --- ordering references must resolve ---------------------------------
	// A `provides` name is an alias a resource answers to, so ordering and
	// dependencies may legitimately reference it.
	alias := map[string]string{}
	for _, e := range entries {
		alias[e.res.Name] = e.res.Name
		for _, p := range e.provides {
			if _, taken := alias[p]; !taken {
				alias[p] = e.res.Name
			}
		}
	}
	for _, e := range entries {
		for _, ref := range e.res.After {
			if _, ok := alias[ref]; !ok {
				add(e.component, "after", fmt.Sprintf("%s must start after %q, which no resource in this composition provides", describe(e.res), ref))
			}
		}
		for _, ref := range e.res.Before {
			if _, ok := alias[ref]; !ok {
				add(e.component, "before", fmt.Sprintf("%s must start before %q, which no resource in this composition provides", describe(e.res), ref))
			}
		}
		for _, ref := range e.res.Dependencies {
			if strings.HasPrefix(ref, "/") {
				continue // an FXServer capability requirement, not a resource
			}
			if _, ok := alias[ref]; !ok {
				add(e.component, "dependencies", fmt.Sprintf("%s depends on %q, which no resource in this composition provides", describe(e.res), ref))
			}
		}
	}
	if len(probs) > 0 {
		return Plan{}, probs
	}

	// --- deterministic ensure order ---------------------------------------
	plan := make([]domain.ReleaseResource, len(entries))
	for i, e := range entries {
		plan[i] = e.res
	}
	// A manifest `dependency` is a real ordering hint, so it is used — but only
	// as a soft one. A dependency loop between two resources is a common and
	// harmless shape in the wild (FXServer resolves it at start time), and it
	// must not be able to block a release. Explicit after/before constraints
	// are what an operator wrote down deliberately, so those are hard: a cycle
	// among them is a genuine contradiction and fails.
	ordered, cyc := topoSort(plan, alias, true)
	if cyc != nil {
		ordered, cyc = topoSort(plan, alias, false)
	}
	if cyc != nil {
		add("", "order", fmt.Sprintf("ordering constraints form a cycle: %s", strings.Join(cyc, " → ")))
		return Plan{}, probs
	}
	for i := range ordered {
		ordered[i].EnsureOrder = i
	}

	rt := []domain.RuntimeResource{}
	if in.Runtime != nil {
		used := map[string]bool{}
		for _, r := range ordered {
			if r.ResourceID != "" {
				used[r.ResourceID] = true
			}
		}
		for _, rr := range in.Runtime.Spec.Resources {
			if used[rr.ID] {
				rt = append(rt, rr)
			}
		}
	}
	return Plan{Resources: ordered, RuntimeResources: rt}, nil
}

func activationFlag(a domain.Activation) string {
	if a == domain.ActivationDisabled {
		return "disable"
	}
	return "enable"
}

func quoteArg(p string) string {
	if p == "" {
		return "<path>"
	}
	if strings.ContainsAny(p, "[]() ") {
		return "'" + p + "'"
	}
	return p
}

// applyOverride replaces only the fields an override is allowed to set, and
// records the new provenance when it actually changed something.
func applyOverride(r *domain.ReleaseResource, activation *domain.Activation, after, before []string, source string) {
	changed := false
	if activation != nil && *activation != "" && *activation != r.Activation {
		r.Activation = *activation
		changed = true
	}
	if after != nil {
		r.After = append([]string(nil), after...)
		changed = true
	}
	if before != nil {
		r.Before = append([]string(nil), before...)
		changed = true
	}
	if changed {
		r.ActivationSource = source
	}
}

func describe(r domain.ReleaseResource) string {
	if r.Source == domain.ResourceSourceComponent {
		return r.RepositoryName + "/" + r.Path
	}
	return r.ResourceID
}

// topoSort produces the ensure order: a stable topological sort of the base
// order under the after/before constraints. Stability matters — two releases
// built from the same sources must produce the same order, and an operator
// reading the diff of two releases should not see unrelated reordering.
func topoSort(res []domain.ReleaseResource, alias map[string]string, withDependencies bool) ([]domain.ReleaseResource, []string) {
	pos := map[string]int{}
	for i, r := range res {
		pos[r.Name] = i
	}
	resolve := func(ref string) (int, bool) {
		name, ok := alias[ref]
		if !ok {
			return 0, false
		}
		i, ok := pos[name]
		return i, ok
	}
	// edges[i] holds the indexes that must start before i.
	edges := make([]map[int]bool, len(res))
	for i := range edges {
		edges[i] = map[int]bool{}
	}
	for i, r := range res {
		for _, ref := range r.After {
			if j, ok := resolve(ref); ok && j != i {
				edges[i][j] = true
			}
		}
		for _, ref := range r.Before {
			if j, ok := resolve(ref); ok && j != i {
				edges[j][i] = true
			}
		}
		if !withDependencies {
			continue
		}
		for _, ref := range r.Dependencies {
			if j, ok := resolve(ref); ok && j != i {
				edges[i][j] = true
			}
		}
	}
	indeg := make([]int, len(res))
	for i := range res {
		indeg[i] = len(edges[i])
	}
	out := make([]domain.ReleaseResource, 0, len(res))
	done := make([]bool, len(res))
	for len(out) < len(res) {
		next := -1
		for i := range res {
			if !done[i] && indeg[i] == 0 {
				next = i
				break // base order is already the tie-break
			}
		}
		if next < 0 {
			return nil, cycleOf(res, edges, done)
		}
		done[next] = true
		out = append(out, res[next])
		for i := range res {
			if !done[i] && edges[i][next] {
				indeg[i]--
			}
		}
	}
	return out, nil
}

// cycleOf names one cycle among the not-yet-emitted resources so the error
// tells the operator which constraints to look at.
func cycleOf(res []domain.ReleaseResource, edges []map[int]bool, done []bool) []string {
	state := make([]int, len(res)) // 0 unvisited, 1 on stack, 2 finished
	var stack []int
	var found []string
	var walk func(i int) bool
	walk = func(i int) bool {
		state[i] = 1
		stack = append(stack, i)
		for j := range edges[i] {
			if done[j] {
				continue
			}
			switch state[j] {
			case 0:
				if walk(j) {
					return true
				}
			case 1:
				at := 0
				for k, v := range stack {
					if v == j {
						at = k
						break
					}
				}
				for _, v := range stack[at:] {
					found = append(found, res[v].Name)
				}
				found = append(found, res[j].Name)
				return true
			}
		}
		state[i] = 2
		stack = stack[:len(stack)-1]
		return false
	}
	for i := range res {
		if !done[i] && state[i] == 0 {
			if walk(i) {
				return found
			}
		}
	}
	return []string{"unresolvable ordering constraints"}
}
