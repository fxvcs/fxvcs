// Package deployment persists health-state transitions and last-known-good
// evidence around the pure health machine. Filesystem current must be observed
// by the caller under the target lock and supplied explicitly.
package deployment

import (
	"context"
	"errors"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/health"
	"fxvcs.dev/fxvcs/internal/state"
)

var ErrCurrentMismatch = errors.New("deployment: filesystem current does not match healthy realization")

type Coordinator struct {
	DB  *state.AgentDB
	Now func() time.Time
}

func (c Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// ApplyEvent loads rollback-cap evidence, applies one authoritative health
// transition, and persists deployment phase/hold/invalidation atomically per
// database operation. observedCurrent must come from activation.ObserveCurrent
// under the target lock; it is required before marking last-known-good.
func (c Coordinator) ApplyEvent(ctx context.Context, dep domain.Deployment, policy health.Policy, event health.Event, observedCurrent string) (domain.Deployment, error) {
	now := c.now()
	since := time.Time{}
	if policy.RollbackWindow > 0 {
		since = now.Add(-policy.RollbackWindow)
	}
	rollbacks, err := c.DB.RollbackTimes(ctx, since)
	if err != nil {
		return dep, err
	}
	current := health.State{
		Phase:              health.Phase(dep.Phase),
		RetryHeld:          dep.RetryHeld,
		Tuple:              dep.RealizationID,
		RollbackTimes:      rollbacks,
		AttemptInvalidated: dep.AttemptInvalidated,
	}
	next, err := health.Transition(current, policy, event, now)
	if err != nil {
		return dep, err
	}
	if next.Phase == health.PhaseActiveHealthy && observedCurrent != dep.RealizationID {
		return dep, ErrCurrentMismatch
	}
	if next.Phase == health.PhaseRollingBack && current.Phase != health.PhaseRollingBack {
		if err := c.DB.RecordRollback(ctx, dep.RealizationID, now); err != nil {
			return dep, err
		}
	}
	dep.Phase = string(next.Phase)
	dep.RetryHeld = next.RetryHeld
	dep.AttemptInvalidated = next.AttemptInvalidated
	if terminal(next.Phase) {
		dep.FinishedAt = now.Format(time.RFC3339Nano)
	}
	if err := c.DB.PutDeployment(ctx, dep); err != nil {
		return dep, err
	}
	if next.Phase == health.PhaseActiveHealthy {
		cached, err := c.DB.TargetState(ctx)
		if err != nil {
			return dep, err
		}
		cached.CurrentRealizationID = observedCurrent
		cached.LastKnownGoodRealizationID = dep.RealizationID
		cached.ObservedAt = now.Format(time.RFC3339Nano)
		if err := c.DB.ObserveTargetState(ctx, cached); err != nil {
			return dep, err
		}
	}
	return dep, nil
}

func terminal(phase health.Phase) bool {
	switch phase {
	case health.PhaseActiveHealthy, health.PhaseRolledBack, health.PhaseActivationFailed, health.PhaseHalted:
		return true
	default:
		return false
	}
}
