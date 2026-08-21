package schema

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/activationwindow"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Semantic checks: rules the JSON Schema cannot express. Each returns the
// issues in document order and stops at nothing (callers see every problem),
// except where a later rule would be meaningless after an earlier failure.

var kebabRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func normalizedPath(path, p string, iss *Issues) bool {
	if _, err := pathsafe.Normalize(p); err != nil {
		*iss = append(*iss, Issue{Path: path, Message: fmt.Sprintf("path %q is not a normalized repository-relative path: %v", p, err), Cause: err})
		return false
	}
	return true
}

func sourceRootOK(path, p string, iss *Issues) bool {
	if p == "." {
		return true
	}
	return normalizedPath(path, p, iss)
}

func mountOK(path, mount string, iss *Issues) bool {
	if !strings.HasPrefix(mount, "resources/") {
		*iss = append(*iss, Issue{Path: path, Message: fmt.Sprintf("mount %q must start with \"resources/\"", mount)})
		return false
	}
	return normalizedPath(path, mount, iss)
}

// orderingGraph checks that every after/before reference names a known
// resource name and that the combined ordering constraints are acyclic.
type orderingGraph struct {
	names map[string]string // name -> JSON pointer of the declaring entry
	edges map[string][]string
	order []string
}

func newOrderingGraph() *orderingGraph {
	return &orderingGraph{names: map[string]string{}, edges: map[string][]string{}}
}

func (g *orderingGraph) declare(path, name string, iss *Issues) {
	if prev, dup := g.names[name]; dup {
		*iss = append(*iss, Issue{Path: path, Message: fmt.Sprintf("duplicate resource name %q (also declared at %s)", name, prev)})
		return
	}
	g.names[name] = path
	g.order = append(g.order, name)
}

// constrain records "name runs after each of after" and "name runs before
// each of before". References are resolved after every entry is declared.
type constraint struct {
	path         string
	name         string
	after, befor []string
}

func (g *orderingGraph) resolve(cs []constraint, iss *Issues, requireKnown bool) {
	for _, c := range cs {
		for i, a := range c.after {
			if _, ok := g.names[a]; !ok {
				if requireKnown {
					*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/after/%d", c.path, i), Message: fmt.Sprintf("after refers to unknown resource name %q", a)})
				}
				continue
			}
			if a == c.name {
				*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/after/%d", c.path, i), Message: fmt.Sprintf("resource %q cannot be ordered after itself", a)})
				continue
			}
			g.edges[a] = append(g.edges[a], c.name) // a -> c
		}
		for i, b := range c.befor {
			if _, ok := g.names[b]; !ok {
				if requireKnown {
					*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/before/%d", c.path, i), Message: fmt.Sprintf("before refers to unknown resource name %q", b)})
				}
				continue
			}
			if b == c.name {
				*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/before/%d", c.path, i), Message: fmt.Sprintf("resource %q cannot be ordered before itself", b)})
				continue
			}
			g.edges[c.name] = append(g.edges[c.name], b) // c -> b
		}
	}
	if cyc := g.cycle(); cyc != nil {
		*iss = append(*iss, Issue{Path: g.names[cyc[0]], Message: "ordering cycle in after/before: " + strings.Join(cyc, " -> ")})
	}
}

// cycle returns one cycle as a name path (first == last) or nil.
func (g *orderingGraph) cycle() []string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		next := append([]string(nil), g.edges[n]...)
		sort.Strings(next)
		for _, m := range next {
			switch color[m] {
			case grey:
				for i, s := range stack {
					if s == m {
						found = append(append([]string(nil), stack[i:]...), m)
						return true
					}
				}
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range g.order {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return nil
}

func checkPersistentPaths(base string, paths []string, iss *Issues) {
	var ok []string
	for i, p := range paths {
		if normalizedPath(fmt.Sprintf("%s/%d", base, i), p, iss) {
			ok = append(ok, p)
		}
	}
	for i := 0; i < len(ok); i++ {
		for j := i + 1; j < len(ok); j++ {
			if pathsafe.Overlaps(ok[i], ok[j]) {
				*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/%d", base, j), Message: fmt.Sprintf("persistent path %q overlaps %q", ok[j], ok[i])})
			}
		}
	}
}

func checkNameList(base string, names []string, iss *Issues) {
	seen := map[string]bool{}
	for i, n := range names {
		if seen[n] {
			*iss = append(*iss, Issue{Path: fmt.Sprintf("%s/%d", base, i), Message: fmt.Sprintf("duplicate entry %q", n)})
		}
		seen[n] = true
	}
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

func checkRepository(r *domain.Repository) Issues {
	var iss Issues
	s := r.Spec
	if s.Profile == "fivem" {
		sourceRootOK("/spec/sourceRoot", s.SourceRoot, &iss)
		if s.Layout == "single" && (s.ResourceName == nil || *s.ResourceName == "") {
			iss = append(iss, Issue{Path: "/spec/resourceName", Message: "resourceName is required for layout single"})
		}
		if s.Layout == "collection" && s.ResourceName != nil {
			iss = append(iss, Issue{Path: "/spec/resourceName", Message: "resourceName is only allowed for layout single"})
		}
	}
	names := map[string]bool{}
	for i, rem := range s.Storage.Remotes {
		p := fmt.Sprintf("/spec/storage/remotes/%d", i)
		if names[rem.Name] {
			iss = append(iss, Issue{Path: p + "/name", Message: fmt.Sprintf("duplicate remote name %q", rem.Name)})
		}
		names[rem.Name] = true
		if strings.Contains(rem.Endpoint, "@") {
			iss = append(iss, Issue{Path: p + "/endpoint", Message: "endpoint must not embed credentials"})
		}
	}
	return iss
}

// ---------------------------------------------------------------------------
// ResourceCatalog
// ---------------------------------------------------------------------------

func checkResourceCatalog(c *domain.ResourceCatalog) Issues {
	var iss Issues
	g := newOrderingGraph()
	var cs []constraint
	paths := map[string]string{}
	for i, r := range c.Spec.Resources {
		p := fmt.Sprintf("/spec/resources/%d", i)
		if normalizedPath(p+"/path", r.Path, &iss) {
			if prev, dup := paths[r.Path]; dup {
				iss = append(iss, Issue{Path: p + "/path", Message: fmt.Sprintf("duplicate resource path %q (also declared at %s)", r.Path, prev)})
			}
			paths[r.Path] = p + "/path"
		}
		g.declare(p+"/name", r.Name, &iss)
		checkNameList(p+"/after", r.After, &iss)
		checkNameList(p+"/before", r.Before, &iss)
		checkNameList(p+"/provides", r.Provides, &iss)
		checkNameList(p+"/dependencies", r.Dependencies, &iss)
		checkPersistentPaths(p+"/persistentPaths", r.PersistentPaths, &iss)
		if r.MutableMode == "copy" && len(r.PersistentPaths) > 0 {
			iss = append(iss, Issue{Path: p + "/persistentPaths", Message: "persistentPaths cannot be combined with mutableMode copy"})
		}
		cs = append(cs, constraint{path: p, name: r.Name, after: r.After, befor: r.Before})
	}
	// A component catalog may order against resources from other components or
	// the runtime, so unknown names are allowed; cycles among known names are not.
	g.resolve(cs, &iss, false)
	return iss
}

// ---------------------------------------------------------------------------
// RuntimeResourceCatalog
// ---------------------------------------------------------------------------

func checkRuntimeResource(p string, r domain.RuntimeResource, iss *Issues) {
	prefix := r.Source + ":"
	if !strings.HasPrefix(r.ID, prefix) {
		*iss = append(*iss, Issue{Path: p + "/id", Message: fmt.Sprintf("resource id %q must start with %q for source %s", r.ID, prefix, r.Source)})
	}
	if r.Source == "runtime" {
		if r.Verifier != nil {
			*iss = append(*iss, Issue{Path: p + "/verifier", Message: "verifier is only allowed on external resources"})
		}
		if r.Lifecycle != nil {
			*iss = append(*iss, Issue{Path: p + "/lifecycle", Message: "lifecycle is only allowed on external resources"})
		}
	}
	if r.Source == "external" {
		if r.Verifier == nil {
			*iss = append(*iss, Issue{Path: p + "/verifier", Message: "external resources require a verifier contract"})
		}
		if r.Lifecycle == nil {
			*iss = append(*iss, Issue{Path: p + "/lifecycle", Message: "external resources require lifecycle requirements"})
		}
	}
	checkNameList(p+"/after", r.After, iss)
	checkNameList(p+"/before", r.Before, iss)
}

func checkRuntimeResourceCatalog(c *domain.RuntimeResourceCatalog) Issues {
	var iss Issues
	g := newOrderingGraph()
	var cs []constraint
	ids := map[string]string{}
	for i, r := range c.Spec.Resources {
		p := fmt.Sprintf("/spec/resources/%d", i)
		if prev, dup := ids[r.ID]; dup {
			iss = append(iss, Issue{Path: p + "/id", Message: fmt.Sprintf("duplicate resource id %q (also declared at %s)", r.ID, prev)})
		}
		ids[r.ID] = p + "/id"
		g.declare(p+"/name", r.Name, &iss)
		checkRuntimeResource(p, r, &iss)
		cs = append(cs, constraint{path: p, name: r.Name, after: r.After, befor: r.Before})
	}
	g.resolve(cs, &iss, false)
	return iss
}

// ---------------------------------------------------------------------------
// ResourceOverrides
// ---------------------------------------------------------------------------

func checkResourceOverrides(o *domain.ResourceOverrides) Issues {
	var iss Issues
	seen := map[string]string{}
	for i, r := range o.Spec.Resources {
		p := fmt.Sprintf("/spec/resources/%d", i)
		component := r.RepositoryID != "" || r.Path != ""
		runtime := r.ResourceID != ""
		var key string
		switch {
		case component && runtime:
			iss = append(iss, Issue{Path: p, Message: "override identity must be exactly one of (repositoryID, path) or resourceID, not both"})
			continue
		case component:
			if r.RepositoryID == "" || r.Path == "" {
				iss = append(iss, Issue{Path: p, Message: "component override identity requires both repositoryID and path"})
				continue
			}
			if !normalizedPath(p+"/path", r.Path, &iss) {
				continue
			}
			key = r.RepositoryID + "\x00" + r.Path
		case runtime:
			key = r.ResourceID
		default:
			iss = append(iss, Issue{Path: p, Message: "override identity must be exactly one of (repositoryID, path) or resourceID"})
			continue
		}
		if prev, dup := seen[key]; dup {
			iss = append(iss, Issue{Path: p, Message: fmt.Sprintf("duplicate override identity (also declared at %s)", prev)})
		}
		seen[key] = p
		if r.Activation == nil && r.After == nil && r.Before == nil {
			iss = append(iss, Issue{Path: p, Message: "override must set at least one of activation, after, before"})
		}
		checkNameList(p+"/after", r.After, &iss)
		checkNameList(p+"/before", r.Before, &iss)
	}
	return iss
}

// ---------------------------------------------------------------------------
// Stack
// ---------------------------------------------------------------------------

func checkDescriptor(p string, d domain.ComponentDescriptor, iss *Issues) {
	sourceRootOK(p+"/sourceRoot", d.SourceRoot, iss)
	if d.Layout == "single" && (d.ResourceName == nil || *d.ResourceName == "") {
		*iss = append(*iss, Issue{Path: p + "/resourceName", Message: "resourceName is required for layout single"})
	}
	if d.Layout == "collection" && d.ResourceName != nil {
		*iss = append(*iss, Issue{Path: p + "/resourceName", Message: "resourceName is only allowed for layout single"})
	}
}

func checkMounts(entries []struct{ path, mount string }, iss *Issues) {
	var ok []struct{ path, mount string }
	for _, e := range entries {
		if mountOK(e.path, e.mount, iss) {
			ok = append(ok, e)
		}
	}
	for i := 0; i < len(ok); i++ {
		for j := i + 1; j < len(ok); j++ {
			if pathsafe.Overlaps(ok[i].mount, ok[j].mount) {
				*iss = append(*iss, Issue{Path: ok[j].path, Message: fmt.Sprintf("mount %q overlaps %q", ok[j].mount, ok[i].mount)})
			}
		}
	}
}

func checkStack(w *domain.Stack) Issues {
	var iss Issues
	ids := map[string]string{}
	aliases := map[string]string{}
	var mounts []struct{ path, mount string }
	for i, c := range w.Spec.Components {
		p := fmt.Sprintf("/spec/components/%d", i)
		if prev, dup := ids[c.RepositoryID]; dup {
			iss = append(iss, Issue{Path: p + "/repositoryID", Message: fmt.Sprintf("duplicate repositoryID %q (also declared at %s)", c.RepositoryID, prev)})
		}
		ids[c.RepositoryID] = p + "/repositoryID"
		if c.Alias != "" {
			if prev, dup := aliases[c.Alias]; dup {
				iss = append(iss, Issue{Path: p + "/alias", Message: fmt.Sprintf("duplicate alias %q (also declared at %s)", c.Alias, prev)})
			}
			aliases[c.Alias] = p + "/alias"
		}
		if strings.Contains(c.Remote, "@") && !strings.HasPrefix(c.Remote, "git@") && !strings.HasPrefix(c.Remote, "ssh://git@") {
			iss = append(iss, Issue{Path: p + "/remote", Message: "remote must not embed credentials"})
		}
		mounts = append(mounts, struct{ path, mount string }{p + "/mount", c.Mount})
		if c.DescriptorOverride != nil {
			checkDescriptor(p+"/descriptorOverride", *c.DescriptorOverride, &iss)
		}
	}
	checkMounts(mounts, &iss)
	return iss
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

func checkRelease(r *domain.Release) Issues {
	var iss Issues
	s := r.Spec

	compIDs := map[string]string{}
	compNames := map[string]string{}
	var mounts []struct{ path, mount string }
	for i, c := range s.Components {
		p := fmt.Sprintf("/spec/components/%d", i)
		if prev, dup := compIDs[c.RepositoryID]; dup {
			iss = append(iss, Issue{Path: p + "/repositoryID", Message: fmt.Sprintf("duplicate repositoryID %q (also declared at %s)", c.RepositoryID, prev)})
		}
		compIDs[c.RepositoryID] = p + "/repositoryID"
		if prev, dup := compNames[c.RepositoryName]; dup {
			iss = append(iss, Issue{Path: p + "/repositoryName", Message: fmt.Sprintf("duplicate resolved repository name %q (also declared at %s); coordination must alias it", c.RepositoryName, prev)})
		}
		compNames[c.RepositoryName] = p + "/repositoryName"
		checkDescriptor(p+"/descriptor", c.Descriptor, &iss)
		mounts = append(mounts, struct{ path, mount string }{p + "/mount", c.Mount})
	}
	checkMounts(mounts, &iss)

	rtIDs := map[string]string{}
	rtNames := map[string]string{}
	for i, rr := range s.RuntimeResources {
		p := fmt.Sprintf("/spec/runtimeResources/%d", i)
		if prev, dup := rtIDs[rr.ID]; dup {
			iss = append(iss, Issue{Path: p + "/id", Message: fmt.Sprintf("duplicate resource id %q (also declared at %s)", rr.ID, prev)})
		}
		rtIDs[rr.ID] = p + "/id"
		rtNames[rr.ID] = rr.Name
		if rr.Activation == domain.ActivationPending {
			iss = append(iss, Issue{Path: p + "/activation", Message: "pending activation cannot enter a Release"})
		}
		checkRuntimeResource(p, rr, &iss)
	}

	g := newOrderingGraph()
	var cs []constraint
	identities := map[string]string{}
	orders := map[int]string{}
	for i, res := range s.Resources {
		p := fmt.Sprintf("/spec/resources/%d", i)
		if res.Activation == domain.ActivationPending {
			iss = append(iss, Issue{Path: p + "/activation", Message: "pending activation cannot enter a Release"})
		}
		var key string
		switch res.Source {
		case "component":
			if res.ResourceID != "" {
				iss = append(iss, Issue{Path: p + "/resourceID", Message: "component resources are identified by repositoryID and path, not resourceID"})
			}
			if res.RepositoryID == "" || res.Path == "" {
				iss = append(iss, Issue{Path: p, Message: "component resources require repositoryID and path"})
				continue
			}
			if !normalizedPath(p+"/path", res.Path, &iss) {
				continue
			}
			if _, ok := compIDs[res.RepositoryID]; !ok {
				iss = append(iss, Issue{Path: p + "/repositoryID", Message: fmt.Sprintf("repositoryID %q is not pinned in spec.components", res.RepositoryID)})
			}
			key = res.RepositoryID + "\x00" + res.Path
			if res.ActivationSource == domain.ActivationSourceRuntime {
				iss = append(iss, Issue{Path: p + "/activationSource", Message: "component resources cannot have activationSource runtime-catalog"})
			}
		case "runtime", "external":
			if res.RepositoryID != "" || res.Path != "" || res.RepositoryName != "" {
				iss = append(iss, Issue{Path: p, Message: fmt.Sprintf("%s resources are identified by resourceID only", res.Source)})
			}
			if res.ResourceID == "" {
				iss = append(iss, Issue{Path: p + "/resourceID", Message: fmt.Sprintf("%s resources require resourceID", res.Source)})
				continue
			}
			if !strings.HasPrefix(res.ResourceID, res.Source+":") {
				iss = append(iss, Issue{Path: p + "/resourceID", Message: fmt.Sprintf("resourceID %q must start with %q", res.ResourceID, res.Source+":")})
			}
			if _, ok := rtIDs[res.ResourceID]; !ok {
				iss = append(iss, Issue{Path: p + "/resourceID", Message: fmt.Sprintf("resourceID %q is not snapshotted in spec.runtimeResources", res.ResourceID)})
			} else if rtNames[res.ResourceID] != res.Name {
				iss = append(iss, Issue{Path: p + "/name", Message: fmt.Sprintf("name %q disagrees with runtimeResources entry %q", res.Name, rtNames[res.ResourceID])})
			}
			key = res.ResourceID
			if res.ActivationSource == domain.ActivationSourceComponent {
				iss = append(iss, Issue{Path: p + "/activationSource", Message: fmt.Sprintf("%s resources cannot have activationSource component", res.Source)})
			}
		default:
			iss = append(iss, Issue{Path: p + "/source", Message: fmt.Sprintf("unknown source %q", res.Source)})
			continue
		}
		if prev, dup := identities[key]; dup {
			iss = append(iss, Issue{Path: p, Message: fmt.Sprintf("duplicate resource identity (also declared at %s)", prev)})
		}
		identities[key] = p
		if prev, dup := orders[res.EnsureOrder]; dup {
			iss = append(iss, Issue{Path: p + "/ensureOrder", Message: fmt.Sprintf("duplicate ensureOrder %d (also used at %s)", res.EnsureOrder, prev)})
		}
		orders[res.EnsureOrder] = p + "/ensureOrder"
		if i > 0 && s.Resources[i-1].EnsureOrder > res.EnsureOrder {
			iss = append(iss, Issue{Path: p + "/ensureOrder", Message: "resources must be listed in ascending ensureOrder"})
		}
		g.declare(p+"/name", res.Name, &iss)
		checkNameList(p+"/after", res.After, &iss)
		checkNameList(p+"/before", res.Before, &iss)
		checkNameList(p+"/dependencies", res.Dependencies, &iss)
		checkNameList(p+"/provides", res.Provides, &iss)
		cs = append(cs, constraint{path: p, name: res.Name, after: res.After, befor: res.Before})
	}
	// A Release is fully resolved: every ordering reference must be known.
	g.resolve(cs, &iss, true)

	if s.VariantOf == r.Metadata.Name {
		iss = append(iss, Issue{Path: "/spec/variantOf", Message: "a release cannot be a variant of itself"})
	}
	if len(iss) == 0 {
		want, err := integrity.SourceSetDigest(s)
		if err != nil {
			iss = append(iss, Issue{Path: "/spec", Message: "sourceSetDigest cannot be computed: " + err.Error(), Cause: err})
		} else if want != s.SourceSetDigest {
			iss = append(iss, Issue{Path: "/spec/sourceSetDigest", Message: fmt.Sprintf("sourceSetDigest %s does not match the pinned sources (expected %s)", s.SourceSetDigest, want)})
		}
	}
	return iss
}

// ---------------------------------------------------------------------------
// Environment / Target
// ---------------------------------------------------------------------------

func checkEnvironment(e *domain.Environment) Issues {
	var iss Issues
	if strings.HasPrefix(e.Spec.SecretScope, "-") {
		iss = append(iss, Issue{Path: "/spec/secretScope", Message: "secretScope must be lowercase kebab-case"})
	}
	return iss
}

func checkInputSource(p string, in domain.InputSource, iss *Issues) {
	n := 0
	if in.Secret != nil {
		n++
		if !kebabRE.MatchString(in.Secret.Namespace) {
			*iss = append(*iss, Issue{Path: p + "/secret/namespace", Message: fmt.Sprintf("secret namespace %q must be lowercase kebab-case", in.Secret.Namespace)})
		}
		if !kebabRE.MatchString(in.Secret.Key) {
			*iss = append(*iss, Issue{Path: p + "/secret/key", Message: fmt.Sprintf("secret key %q must be lowercase kebab-case", in.Secret.Key)})
		}
	}
	if in.Target != nil {
		n++
	}
	if in.Release != nil {
		n++
	}
	if in.Literal != nil {
		n++
	}
	if n != 1 {
		*iss = append(*iss, Issue{Path: p, Message: fmt.Sprintf("input source must have exactly one of secret, target, release, literal (found %d)", n)})
	}
}

func checkHooks(phase string, hooks []domain.HookRequest, ids map[string]string, iss *Issues) {
	for i, h := range hooks {
		p := fmt.Sprintf("/spec/hooks/%s/%d", phase, i)
		if prev, dup := ids[h.ID]; dup {
			*iss = append(*iss, Issue{Path: p + "/id", Message: fmt.Sprintf("duplicate hook id %q (also declared at %s)", h.ID, prev)})
		}
		ids[h.ID] = p + "/id"
		if h.Type == format.TemplateRendererVersion {
			if h.Template == nil {
				*iss = append(*iss, Issue{Path: p + "/template", Message: "template.v1 hooks require a pinned template"})
			} else {
				normalizedPath(p+"/template/path", h.Template.Path, iss)
			}
			if h.Destination == "" {
				*iss = append(*iss, Issue{Path: p + "/destination", Message: "template.v1 hooks require a destination"})
			} else {
				normalizedPath(p+"/destination", h.Destination, iss)
			}
		}
		names := make([]string, 0, len(h.Inputs))
		for name := range h.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			checkInputSource(p+"/inputs/"+escapePointer(name), h.Inputs[name], iss)
		}
	}
}

func checkTarget(t *domain.Target) Issues {
	var iss Issues
	s := t.Spec
	if s.Health != nil {
		if len(s.Health.Checks) == 0 {
			iss = append(iss, Issue{Path: "/spec/health/checks", Message: "health policy requires at least one check"})
		}
		if s.Health.ActivationWindow != nil {
			if _, err := activationwindow.Parse(*s.Health.ActivationWindow); err != nil {
				iss = append(iss, Issue{Path: "/spec/health/activationWindow", Message: err.Error(), Cause: err})
			}
		}
		for i, c := range s.Health.Checks {
			if c.Type != "fivem-http" && len(c.Endpoints) > 0 {
				iss = append(iss, Issue{Path: fmt.Sprintf("/spec/health/checks/%d/endpoints", i), Message: "endpoints are only allowed on fivem-http checks"})
			}
		}
	}
	if s.Hooks != nil {
		ids := map[string]string{}
		checkHooks("postMaterialize", s.Hooks.PostMaterialize, ids, &iss)
		checkHooks("preActivate", s.Hooks.PreActivate, ids, &iss)
		checkHooks("postActivate", s.Hooks.PostActivate, ids, &iss)
		checkHooks("onFailure", s.Hooks.OnFailure, ids, &iss)
	}
	if s.Trust != nil {
		ct := s.Trust.Coordination
		if ct.AllowedSignersFile != "" {
			normalizedPath("/spec/trust/coordination/allowedSignersFile", ct.AllowedSignersFile, &iss)
		}
		if ct.Authority == "signed-commit-tip" && ct.PromotionTagPrefix != "" {
			iss = append(iss, Issue{Path: "/spec/trust/coordination/promotionTagPrefix", Message: "promotionTagPrefix is only meaningful under signed-promotion-tag authority"})
		}
	}
	return iss
}

// ---------------------------------------------------------------------------
// AgentConfig
// ---------------------------------------------------------------------------

var secretAliasRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func cleanAbsolute(path, value string, iss *Issues) bool {
	// Agent configs are often authored or validated on a different OS from
	// their target. Accept clean POSIX paths on Windows as well as native paths
	// so semantic validation does not depend on the validator's host OS.
	native := filepath.IsAbs(value) && filepath.Clean(value) == value
	posix := pathpkg.IsAbs(value) && pathpkg.Clean(value) == value
	if value == "" || (!native && !posix) {
		*iss = append(*iss, Issue{Path: path, Message: fmt.Sprintf("path %q must be absolute and clean", value), Cause: layout.ErrInvalidRoot})
		return false
	}
	return true
}

func checkAgentConfig(c *domain.AgentConfig) Issues {
	var iss Issues
	seenTargets := map[string]string{}
	seenRewrites := map[string]string{}
	for i, rewrite := range c.Spec.Git.RemoteRewrite {
		p := fmt.Sprintf("/spec/git/remoteRewrite/%d", i)
		if prev, ok := seenRewrites[rewrite.From]; ok {
			iss = append(iss, Issue{Path: p + "/from", Message: fmt.Sprintf("duplicate remote rewrite source %q (also declared at %s)", rewrite.From, prev)})
		}
		seenRewrites[rewrite.From] = p + "/from"
	}

	deployRoots := make([]string, 0, len(c.Spec.Targets))
	for i, target := range c.Spec.Targets {
		p := fmt.Sprintf("/spec/targets/%d", i)
		if prev, ok := seenTargets[target.Target]; ok {
			iss = append(iss, Issue{Path: p + "/target", Message: fmt.Sprintf("duplicate target %q (also declared at %s)", target.Target, prev)})
		}
		seenTargets[target.Target] = p + "/target"

		deployOK := cleanAbsolute(p+"/deployRoot", target.DeployRoot, &iss)
		serverOK := cleanAbsolute(p+"/serverData", target.ServerData, &iss)
		txOK := true
		if target.Runtime.TxData != "" {
			txOK = cleanAbsolute(p+"/runtime/txData", target.Runtime.TxData, &iss)
		}
		systemResourcesOK := true
		if target.Runtime.SystemResourcesRoot != "" {
			systemResourcesOK = cleanAbsolute(p+"/runtime/systemResourcesRoot", target.Runtime.SystemResourcesRoot, &iss)
		}
		if deployOK {
			deployRoots = append(deployRoots, target.DeployRoot)
		}
		if deployOK && serverOK && txOK {
			if err := (layout.Roots{DeployRoot: target.DeployRoot, ServerData: target.ServerData, TxData: target.Runtime.TxData}).Validate(); err != nil {
				iss = append(iss, Issue{Path: p, Message: err.Error(), Cause: err})
			}
		}
		if deployOK && systemResourcesOK && target.Runtime.SystemResourcesRoot != "" && (pathsafe.Within(target.DeployRoot, target.Runtime.SystemResourcesRoot) || pathsafe.Within(target.Runtime.SystemResourcesRoot, target.DeployRoot)) {
			iss = append(iss, Issue{Path: p + "/runtime/systemResourcesRoot", Message: "systemResourcesRoot must not overlap deployRoot"})
		}
		if endpoint := target.Runtime.ResourceState; endpoint != nil && endpoint.Host != "127.0.0.1" && endpoint.Host != "::1" {
			iss = append(iss, Issue{Path: p + "/runtime/resourceState/host", Message: "resource-state endpoint must use a numeric loopback address"})
		}

		switch target.Runtime.Supervisor {
		case "txadmin":
			if target.Runtime.TxData == "" {
				iss = append(iss, Issue{Path: p + "/runtime/txData", Message: "txadmin supervisor requires txData"})
			}
			if target.Runtime.SystemdUnit != "" {
				iss = append(iss, Issue{Path: p + "/runtime/systemdUnit", Message: "systemdUnit is only allowed for systemd supervisor"})
			}
		case "systemd":
			if target.Runtime.SystemdUnit == "" {
				iss = append(iss, Issue{Path: p + "/runtime/systemdUnit", Message: "systemd supervisor requires systemdUnit"})
			}
			if target.Runtime.TxData != "" {
				iss = append(iss, Issue{Path: p + "/runtime/txData", Message: "txData is only allowed for txadmin supervisor"})
			}
		case "manual":
			if target.Runtime.SystemdUnit != "" {
				iss = append(iss, Issue{Path: p + "/runtime/systemdUnit", Message: "systemdUnit is only allowed for systemd supervisor"})
			}
			if target.Runtime.TxData != "" {
				iss = append(iss, Issue{Path: p + "/runtime/txData", Message: "txData is only allowed for txadmin supervisor"})
			}
		}

		providerNames := make([]string, 0, len(target.Secrets))
		for name := range target.Secrets {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
		for _, name := range providerNames {
			provider := target.Secrets[name]
			pp := p + "/secrets/" + escapePointer(name)
			switch provider.Type {
			case "env":
				if provider.Path != "" {
					iss = append(iss, Issue{Path: pp + "/path", Message: "path is only allowed for envfile providers"})
				}
			case "envfile":
				cleanAbsolute(pp+"/path", provider.Path, &iss)
			case "fxvcs-cloud":
				iss = append(iss, Issue{Path: pp + "/type", Message: "fxvcs-cloud secret providers are reserved and not implemented in Stage 4"})
			}
			for ref, alias := range provider.Aliases {
				if !secretAliasRE.MatchString(alias) {
					iss = append(iss, Issue{Path: pp + "/aliases/" + escapePointer(ref), Message: fmt.Sprintf("alias %q must be an environment variable identifier", alias)})
				}
			}
		}

		hookNames := make([]string, 0, len(target.Hooks))
		for name := range target.Hooks {
			hookNames = append(hookNames, name)
		}
		sort.Strings(hookNames)
		for _, name := range hookNames {
			hook := target.Hooks[name]
			hp := p + "/hooks/" + escapePointer(name)
			if normalizedPath(hp+"/allowedDestinationRoot", hook.AllowedDestRoot, &iss) && (hook.AllowedDestRoot == layout.ResourcesDir || strings.HasPrefix(hook.AllowedDestRoot, layout.ResourcesDir+"/")) {
				iss = append(iss, Issue{Path: hp + "/allowedDestinationRoot", Message: "hook destination root must be outside resources/"})
			}
		}
	}

	if err := layout.DistinctDeployRoots(deployRoots...); err != nil {
		iss = append(iss, Issue{Path: "/spec/targets", Message: err.Error(), Cause: err})
	}
	return iss
}

// ---------------------------------------------------------------------------
// Low-level formats
// ---------------------------------------------------------------------------

func checkPointer(p *PointerDocument) Issues {
	ptr := pointer.Pointer{StorageDomainID: p.Domain, Encoding: p.Encoding, ManifestDigest: p.Manifest, OID: p.OID, Size: p.Size}
	if err := ptr.Validate(); err != nil {
		return Issues{{Message: err.Error(), Cause: err}}
	}
	if _, err := ptr.Encode(); err != nil {
		return Issues{{Message: err.Error(), Cause: err}}
	}
	return nil
}

func checkObjectManifest(m *domain.ObjectManifest) Issues {
	if _, err := integrity.ManifestDigest(*m); err != nil {
		return Issues{{Message: err.Error(), Cause: err}}
	}
	return nil
}

func checkAssetIndex(idx *domain.AssetIndex) Issues {
	var iss Issues
	seen := map[string]bool{}
	for i, e := range idx.Entries {
		p := fmt.Sprintf("/entries/%d", i)
		normalizedPath(p+"/path", e.Path, &iss)
		if seen[e.Path] {
			iss = append(iss, Issue{Path: p + "/path", Message: fmt.Sprintf("duplicate entry path %q", e.Path)})
		}
		seen[e.Path] = true
		if i > 0 && idx.Entries[i-1].Path >= e.Path {
			iss = append(iss, Issue{Path: p + "/path", Message: fmt.Sprintf("entries must be strictly sorted by path in byte order (%q >= %q)", idx.Entries[i-1].Path, e.Path)})
		}
	}
	if len(iss) == 0 {
		if _, err := integrity.AssetRoot(*idx); err != nil {
			iss = append(iss, Issue{Message: err.Error(), Cause: err})
		}
	}
	return iss
}
