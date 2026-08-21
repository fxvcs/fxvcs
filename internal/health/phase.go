// Package health implements the deployment phase / retry-suppression state
// machine from docs/spec/health-and-rollback.md as a pure function so the
// contract can be fixture-tested without a running agent.
//
// Phase and retryHeld are separate. A failed realization-input tuple is held
// until desired state changes, an authorized retry clears it, or (only for
// active-unhealthy under keep) the same tuple later reaches healthy soak.
package health

import (
	"errors"
	"fmt"
	"time"
)

// Phase is the observed deployment phase.
type Phase string

const (
	PhaseIdle             Phase = "idle" // no realization attempted for this tuple yet
	PhaseStaging          Phase = "staging"
	PhaseActiveGrace      Phase = "active-grace" // activated, within startupGrace / thresholds / soak
	PhaseActiveHealthy    Phase = "active-healthy"
	PhaseActiveUnhealthy  Phase = "active-unhealthy"
	PhaseRollingBack      Phase = "rolling-back" // rollback realization being staged/activated
	PhaseRolledBack       Phase = "rolled-back"
	PhaseActivationFailed Phase = "activation-failed"
	PhaseHalted           Phase = "halted" // rollback also failed; operator intervention required
)

// OnFailure policy.
const (
	OnFailureRollback = "rollback"
	OnFailureKeep     = "keep"
	OnFailureNone     = "none"
)

// Event is an input to the machine.
type Event string

const (
	EventStagingStarted    Event = "staging-started"
	EventActivated         Event = "activated"           // pointer switched to desired tuple
	EventPreActivateFailed Event = "pre-activate-failed" // staging/validation/hook failure before activation
	EventHealthy           Event = "healthy"             // thresholds + soak satisfied
	EventUnhealthy         Event = "unhealthy"           // failureThreshold reached
	EventRollbackActivated Event = "rollback-activated"  // rollback realization active and healthy
	EventRollbackFailed    Event = "rollback-failed"
	EventDesiredChanged    Event = "desired-changed"    // new tuple authorized
	EventOperatorRetry     Event = "operator-retry"     // fxvcs agent retry <target> (audited)
	EventSupervisorRestart Event = "supervisor-restart" // process generation changed outside adapter
)

// Policy is the subset of HealthPolicy the machine needs.
type Policy struct {
	OnFailure             string
	MaxAutomaticRollbacks int
	RollbackWindow        time.Duration
	// LocalMaxAutomaticRollbacks is the agent-local floor; desired state
	// cannot raise the effective cap above it. 0 means "no automatic rollback".
	LocalMaxAutomaticRollbacks int
	LocalRollbackDisabled      bool
}

// State is the persisted machine state for one target.
type State struct {
	Phase     Phase
	RetryHeld bool
	// Tuple identifies the realization inputs the phase/hold refer to.
	Tuple string
	// RollbackTimes records automatic rollbacks for cap accounting.
	RollbackTimes []time.Time
	// AttemptInvalidated is set when a competing supervisor restart raced an
	// activation attempt; the attempt must be retried under bounded policy.
	AttemptInvalidated bool
}

// Errors.
var (
	ErrInvalidTransition = errors.New("health: invalid transition")
	ErrRetryHeld         = errors.New("health: realization tuple is retry-held")
	ErrInvalidPolicy     = errors.New("health: invalid policy")
)

// ValidatePolicy checks enum values and non-negative caps.
func ValidatePolicy(p Policy) error {
	switch p.OnFailure {
	case OnFailureRollback, OnFailureKeep, OnFailureNone:
	default:
		return fmt.Errorf("%w: onFailure %q", ErrInvalidPolicy, p.OnFailure)
	}
	if p.MaxAutomaticRollbacks < 0 || p.LocalMaxAutomaticRollbacks < 0 || p.RollbackWindow < 0 {
		return fmt.Errorf("%w: negative cap", ErrInvalidPolicy)
	}
	return nil
}

// EffectiveRollbackCap applies the agent-local floor: min(desired, local), and
// zero when the agent disables automatic rollback.
func EffectiveRollbackCap(p Policy) int {
	if p.LocalRollbackDisabled {
		return 0
	}
	limit := p.MaxAutomaticRollbacks
	if p.LocalMaxAutomaticRollbacks < limit {
		limit = p.LocalMaxAutomaticRollbacks
	}
	return limit
}

// CanStart reports whether a reconcile of tuple may begin. It refuses a held
// tuple: the agent must not re-attempt a failed tuple until desired state
// changes or an operator retries.
func CanStart(s State, tuple string) error {
	if s.RetryHeld && s.Tuple == tuple {
		return fmt.Errorf("%w: %s", ErrRetryHeld, tuple)
	}
	return nil
}

// Transition applies ev at time now and returns the new state. It is total
// over the documented (phase, event) pairs and rejects everything else.
func Transition(s State, p Policy, ev Event, now time.Time) (State, error) {
	if err := ValidatePolicy(p); err != nil {
		return s, err
	}
	next := s
	switch ev {
	case EventDesiredChanged:
		// A new authorized tuple always clears the hold for the old tuple.
		next.RetryHeld = false
		next.AttemptInvalidated = false
		if s.Phase == PhaseStaging || s.Phase == PhaseActiveGrace {
			return s, fmt.Errorf("%w: desired changed during %s must abort the attempt first", ErrInvalidTransition, s.Phase)
		}
		return next, nil
	case EventOperatorRetry:
		if !s.RetryHeld {
			return s, fmt.Errorf("%w: operator retry with no hold", ErrInvalidTransition)
		}
		next.RetryHeld = false
		return next, nil
	}

	switch s.Phase {
	case PhaseIdle, PhaseActiveHealthy, PhaseRolledBack, PhaseActivationFailed, PhaseActiveUnhealthy:
		if ev == EventStagingStarted {
			if s.RetryHeld && s.Phase != PhaseActiveUnhealthy {
				return s, fmt.Errorf("%w: cannot stage held tuple", ErrRetryHeld)
			}
			if s.Phase == PhaseActiveUnhealthy && s.RetryHeld {
				// Only a *different* tuple may stage; same tuple keeps evaluating health.
				return s, fmt.Errorf("%w: active-unhealthy tuple is held; only health may clear it", ErrRetryHeld)
			}
			next.Phase = PhaseStaging
			next.AttemptInvalidated = false
			return next, nil
		}
		if s.Phase == PhaseActiveUnhealthy {
			switch ev {
			case EventHealthy:
				// Late recovery of the exact tuple clears the hold automatically.
				next.Phase = PhaseActiveHealthy
				next.RetryHeld = false
				return next, nil
			case EventUnhealthy:
				return next, nil // stays unhealthy; hold unchanged
			case EventSupervisorRestart:
				return next, nil // health continues; generation re-observed
			}
		}
		if s.Phase == PhaseActiveHealthy {
			switch ev {
			case EventHealthy:
				return next, nil
			case EventUnhealthy:
				// Post-soak degradation is monitoring, not deployment failure:
				// the last-known-good realization stays; no automatic rollback.
				return next, nil
			case EventSupervisorRestart:
				return next, nil
			}
		}
	case PhaseStaging:
		switch ev {
		case EventActivated:
			next.Phase = PhaseActiveGrace
			return next, nil
		case EventPreActivateFailed:
			next.Phase = PhaseActivationFailed
			next.RetryHeld = true
			return next, nil
		case EventSupervisorRestart:
			next.AttemptInvalidated = true
			return next, nil
		}
	case PhaseActiveGrace:
		switch ev {
		case EventHealthy:
			if s.AttemptInvalidated {
				return s, fmt.Errorf("%w: invalidated attempt cannot become healthy", ErrInvalidTransition)
			}
			next.Phase = PhaseActiveHealthy
			next.RetryHeld = false
			return next, nil
		case EventSupervisorRestart:
			// A competing restart during grace/soak invalidates the attempt; it
			// is never treated as a successful activation.
			next.AttemptInvalidated = true
			return next, nil
		case EventUnhealthy:
			switch p.OnFailure {
			case OnFailureKeep:
				next.Phase = PhaseActiveUnhealthy
				next.RetryHeld = true
				return next, nil
			case OnFailureNone:
				next.Phase = PhaseActiveUnhealthy
				next.RetryHeld = true
				return next, nil
			case OnFailureRollback:
				if !rollbackAllowed(s, p, now) {
					// Cap reached: preserve the safest state we can identify and stop.
					next.Phase = PhaseHalted
					next.RetryHeld = true
					return next, nil
				}
				next.RollbackTimes = append(append([]time.Time(nil), s.RollbackTimes...), now)
				next.Phase = PhaseRollingBack
				next.RetryHeld = true
				return next, nil
			}
		}
	case PhaseRollingBack:
		switch ev {
		case EventRollbackActivated:
			// A rollback is a new realization of the prior Release; it must pass
			// its own health before the phase reports rolled-back.
			next.Phase = PhaseRolledBack
			return next, nil
		case EventRollbackFailed, EventUnhealthy:
			// The rollback realization also failed: stop, preserve the safest
			// identifiable state, require operator intervention. Never oscillate.
			next.Phase = PhaseHalted
			next.RetryHeld = true
			return next, nil
		case EventSupervisorRestart:
			next.AttemptInvalidated = true
			return next, nil
		}
	case PhaseHalted:
		// Only desired-changed / operator-retry (handled above) leave halted.
	}
	return s, fmt.Errorf("%w: %s + %s", ErrInvalidTransition, s.Phase, ev)
}

func rollbackAllowed(s State, p Policy, now time.Time) bool {
	limit := EffectiveRollbackCap(p)
	if limit <= 0 {
		return false
	}
	n := 0
	for _, t := range s.RollbackTimes {
		if p.RollbackWindow == 0 || now.Sub(t) <= p.RollbackWindow {
			n++
		}
	}
	return n < limit
}
