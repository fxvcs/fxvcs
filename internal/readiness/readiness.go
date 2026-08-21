// Package readiness runs independently bounded typed checks and evaluates
// startup grace, consecutive thresholds, and soak without owning deployment
// lifecycle transitions.
package readiness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
)

type Decision string

const (
	Pending   Decision = "pending"
	Healthy   Decision = "healthy"
	Unhealthy Decision = "unhealthy"
)

type Policy struct {
	StartupGrace     time.Duration
	Timeout          time.Duration
	SuccessThreshold int
	FailureThreshold int
	SoakDuration     time.Duration
}

func PolicyFromDomain(policy domain.HealthPolicy) (Policy, error) {
	parse := func(name, value string) (time.Duration, error) {
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("readiness: invalid %s %q", name, value)
		}
		return d, nil
	}
	startup, err := parse("startupGrace", policy.StartupGrace)
	if err != nil {
		return Policy{}, err
	}
	timeout, err := parse("timeout", policy.Timeout)
	if err != nil {
		return Policy{}, err
	}
	soak, err := parse("soakDuration", policy.SoakDuration)
	if err != nil {
		return Policy{}, err
	}
	if timeout <= 0 || policy.SuccessThreshold <= 0 || policy.FailureThreshold <= 0 {
		return Policy{}, errors.New("readiness: timeout and thresholds must be positive")
	}
	return Policy{StartupGrace: startup, Timeout: timeout, SuccessThreshold: policy.SuccessThreshold, FailureThreshold: policy.FailureThreshold, SoakDuration: soak}, nil
}

// Tracker is the durable/reconstructable threshold state for one activation.
type Tracker struct {
	ActivatedAt          time.Time
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
	HealthySince         time.Time
	Decision             Decision
}

// Observe applies one complete check batch. A batch passes only when every
// configured check passed. Failures during startup grace reset success/soak
// but do not count toward failureThreshold.
func Observe(state Tracker, policy Policy, at time.Time, allPassed bool) (Tracker, error) {
	if state.ActivatedAt.IsZero() || at.Before(state.ActivatedAt) {
		return state, errors.New("readiness: observation precedes activation")
	}
	if state.Decision == Healthy || state.Decision == Unhealthy {
		return state, nil
	}
	next := state
	if allPassed {
		next.ConsecutiveSuccesses++
		next.ConsecutiveFailures = 0
		if next.ConsecutiveSuccesses >= policy.SuccessThreshold {
			if next.HealthySince.IsZero() {
				next.HealthySince = at
			}
			if at.Sub(next.HealthySince) >= policy.SoakDuration {
				next.Decision = Healthy
			}
		}
		return next, nil
	}
	next.ConsecutiveSuccesses = 0
	next.HealthySince = time.Time{}
	if at.Sub(next.ActivatedAt) < policy.StartupGrace {
		next.ConsecutiveFailures = 0
		return next, nil
	}
	next.ConsecutiveFailures++
	if next.ConsecutiveFailures >= policy.FailureThreshold {
		next.Decision = Unhealthy
	}
	return next, nil
}

type Evidence struct {
	Type      string
	Interface string
	Success   bool
	Latency   time.Duration
	Detail    string
}

type Checker interface {
	Check(context.Context, domain.HealthCheck) Evidence
}

// Runner maps each health-check type to a typed implementation. Unknown types
// fail closed as evidence rather than being skipped.
type Runner struct {
	Checkers map[string]Checker
	Now      func() time.Time
}

func (r Runner) Run(ctx context.Context, checks []domain.HealthCheck, timeout time.Duration) []Evidence {
	if r.Now == nil {
		r.Now = time.Now
	}
	type indexed struct {
		index int
		value Evidence
	}
	ch := make(chan indexed, len(checks))
	for i, check := range checks {
		checker := r.Checkers[check.Type]
		go func(index int, definition domain.HealthCheck, implementation Checker) {
			if implementation == nil {
				ch <- indexed{index: index, value: Evidence{Type: definition.Type, Interface: definition.Type, Detail: "unsupported check type"}}
				return
			}
			bounded, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			started := r.Now()
			result := implementation.Check(bounded, definition)
			result.Type = definition.Type
			result.Latency = r.Now().Sub(started)
			if bounded.Err() != nil {
				result.Success = false
				result.Detail = bounded.Err().Error()
			}
			ch <- indexed{index: index, value: result}
		}(i, check, checker)
	}
	results := make([]indexed, 0, len(checks))
	for range checks {
		results = append(results, <-ch)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })
	out := make([]Evidence, len(results))
	for i := range results {
		out[i] = results[i].value
	}
	return out
}

func AllPassed(evidence []Evidence) bool {
	if len(evidence) == 0 {
		return false
	}
	for _, item := range evidence {
		if !item.Success {
			return false
		}
	}
	return true
}
