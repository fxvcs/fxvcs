package migrate

import (
	"errors"
	"fmt"
	"maps"

	"fxvcs.dev/fxvcs/internal/format"
)

// DocMigration converts a committed document from one apiVersion to the next.
// Apply receives a deep-enough copy of the generic document (top-level map is
// copied; nested values are shared, so Apply must not mutate nested values in
// place) and returns the converted document. ApplyDoc sets apiVersion itself
// after each step, so Apply may leave it untouched.
type DocMigration struct {
	FromAPIVersion string
	ToAPIVersion   string
	Description    string
	Apply          func(doc map[string]any) (map[string]any, error)
}

// DocRegistry is a validated ordered chain of document migrations.
type DocRegistry struct {
	steps []DocMigration
	index map[string]int // apiVersion -> position in chain (len(steps) for the last To)
}

// NewDocRegistry validates that ms form one contiguous chain.
func NewDocRegistry(ms ...DocMigration) (*DocRegistry, error) {
	if len(ms) == 0 {
		return nil, fmt.Errorf("%w: empty document registry", ErrGap)
	}
	r := &DocRegistry{index: map[string]int{}}
	for i, m := range ms {
		if m.FromAPIVersion == "" || m.ToAPIVersion == "" || m.FromAPIVersion == m.ToAPIVersion {
			return nil, fmt.Errorf("%w: document migration %d has invalid versions %q -> %q", ErrGap, i, m.FromAPIVersion, m.ToAPIVersion)
		}
		if m.Apply == nil {
			return nil, fmt.Errorf("migrate: document migration %s -> %s has nil Apply", m.FromAPIVersion, m.ToAPIVersion)
		}
		if i > 0 && m.FromAPIVersion != ms[i-1].ToAPIVersion {
			return nil, fmt.Errorf("%w: expected document migration from %s, got %s -> %s", ErrGap, ms[i-1].ToAPIVersion, m.FromAPIVersion, m.ToAPIVersion)
		}
		if _, dup := r.index[m.FromAPIVersion]; dup {
			return nil, fmt.Errorf("%w: apiVersion %s appears twice", ErrGap, m.FromAPIVersion)
		}
		r.index[m.FromAPIVersion] = i
	}
	r.steps = append([]DocMigration(nil), ms...)
	r.index[ms[len(ms)-1].ToAPIVersion] = len(ms)
	return r, nil
}

// Latest is the apiVersion the chain reaches.
func (r *DocRegistry) Latest() string { return r.steps[len(r.steps)-1].ToAPIVersion }

// PlanDoc returns the ordered steps from current to target, or:
//
//   - ErrNewerThanSupported when current is later in the chain than target, or
//     is outside the chain and format.ParseAPIVersion classifies it as newer;
//   - ErrUnknownVersion when current is not in the chain and not newer;
//   - ErrGap when target is not in the chain.
func (r *DocRegistry) PlanDoc(current, target string) ([]DocMigration, error) {
	ti, ok := r.index[target]
	if !ok {
		return nil, fmt.Errorf("%w: target apiVersion %q not in chain", ErrGap, target)
	}
	ci, ok := r.index[current]
	if !ok {
		if _, _, err := format.ParseAPIVersion(current); errors.Is(err, format.ErrNewerAPIVersion) {
			return nil, fmt.Errorf("%w: %v", ErrNewerThanSupported, err)
		}
		return nil, fmt.Errorf("%w: apiVersion %q", ErrUnknownVersion, current)
	}
	switch {
	case ci == ti:
		return nil, nil
	case ci > ti:
		return nil, fmt.Errorf("%w: document is %s, target %s", ErrNewerThanSupported, current, target)
	}
	return append([]DocMigration(nil), r.steps[ci:ti]...), nil
}

// ApplyDoc runs the plan over doc and returns the converted document. The
// input map is not modified; on any error nothing is returned, so callers can
// write the result only after the whole chain succeeded.
func ApplyDoc(doc map[string]any, plan []DocMigration) (map[string]any, error) {
	cur := maps.Clone(doc)
	if cur == nil {
		cur = map[string]any{}
	}
	for _, step := range plan {
		if got, _ := cur["apiVersion"].(string); got != step.FromAPIVersion {
			return nil, fmt.Errorf("%w: document apiVersion %q, step expects %s", ErrVersionMismatch, got, step.FromAPIVersion)
		}
		next, err := step.Apply(maps.Clone(cur))
		if err != nil {
			return nil, fmt.Errorf("migrate: %s -> %s (%s): %w", step.FromAPIVersion, step.ToAPIVersion, step.Description, err)
		}
		if next == nil {
			return nil, fmt.Errorf("migrate: %s -> %s returned nil document", step.FromAPIVersion, step.ToAPIVersion)
		}
		next["apiVersion"] = step.ToAPIVersion
		cur = next
	}
	return cur, nil
}
