package health

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func pol(onFailure string) Policy {
	return Policy{OnFailure: onFailure, MaxAutomaticRollbacks: 1, RollbackWindow: 30 * time.Minute, LocalMaxAutomaticRollbacks: 5}
}

func step(t *testing.T, s State, p Policy, ev Event, now time.Time) State {
	t.Helper()
	n, err := Transition(s, p, ev, now)
	if err != nil {
		t.Fatalf("%s + %s: %v", s.Phase, ev, err)
	}
	return n
}

func TestHappyPathAndSoak(t *testing.T) {
	s := State{Phase: PhaseIdle, Tuple: "t1"}
	s = step(t, s, pol(OnFailureKeep), EventStagingStarted, t0)
	s = step(t, s, pol(OnFailureKeep), EventActivated, t0)
	if s.Phase != PhaseActiveGrace {
		t.Fatal(s.Phase)
	}
	s = step(t, s, pol(OnFailureKeep), EventHealthy, t0)
	if s.Phase != PhaseActiveHealthy || s.RetryHeld {
		t.Fatalf("%+v", s)
	}
	// Post-soak unhealthy is monitoring; no rollback, no hold.
	s = step(t, s, pol(OnFailureRollback), EventUnhealthy, t0)
	if s.Phase != PhaseActiveHealthy || s.RetryHeld {
		t.Fatalf("post-soak degradation changed deployment phase: %+v", s)
	}
}

func TestKeepHoldsAndLateRecoveryClears(t *testing.T) {
	p := pol(OnFailureKeep)
	s := State{Phase: PhaseIdle, Tuple: "t1"}
	s = step(t, s, p, EventStagingStarted, t0)
	s = step(t, s, p, EventActivated, t0)
	s = step(t, s, p, EventUnhealthy, t0)
	if s.Phase != PhaseActiveUnhealthy || !s.RetryHeld {
		t.Fatalf("%+v", s)
	}
	if err := CanStart(s, "t1"); !errors.Is(err, ErrRetryHeld) {
		t.Fatal("held tuple allowed to start")
	}
	if err := CanStart(s, "t2"); err != nil {
		t.Fatal("different tuple blocked")
	}
	if _, err := Transition(s, p, EventStagingStarted, t0); !errors.Is(err, ErrRetryHeld) {
		t.Fatal("re-staging held tuple accepted")
	}
	// Health fluctuation does not clear the hold; healthy+soak does.
	s = step(t, s, p, EventUnhealthy, t0)
	if !s.RetryHeld {
		t.Fatal("hold cleared by unhealthy")
	}
	s = step(t, s, p, EventHealthy, t0)
	if s.Phase != PhaseActiveHealthy || s.RetryHeld {
		t.Fatalf("late recovery did not clear hold: %+v", s)
	}
}

func TestNoneReportsUnhealthyWithoutRollbackAndCanRecover(t *testing.T) {
	p := pol(OnFailureNone)
	s := State{Phase: PhaseIdle, Tuple: "t1"}
	s = step(t, s, p, EventStagingStarted, t0)
	s = step(t, s, p, EventActivated, t0)
	s = step(t, s, p, EventUnhealthy, t0)
	if s.Phase != PhaseActiveUnhealthy || !s.RetryHeld || len(s.RollbackTimes) != 0 {
		t.Fatalf("none performed recovery action: %+v", s)
	}
	s = step(t, s, p, EventHealthy, t0.Add(time.Minute))
	if s.Phase != PhaseActiveHealthy || s.RetryHeld {
		t.Fatalf("none late recovery failed: %+v", s)
	}
}

func TestPreActivationFailureLeavesPriorActiveAndHolds(t *testing.T) {
	p := pol(OnFailureRollback)
	s := State{Phase: PhaseActiveHealthy, Tuple: "t2"}
	s = step(t, s, p, EventStagingStarted, t0)
	s = step(t, s, p, EventPreActivateFailed, t0)
	if s.Phase != PhaseActivationFailed || !s.RetryHeld || len(s.RollbackTimes) != 0 {
		t.Fatalf("%+v", s)
	}
	// Only desired change or operator retry clears it.
	if _, err := Transition(s, p, EventHealthy, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("health event accepted in activation-failed")
	}
	s2 := step(t, s, p, EventOperatorRetry, t0)
	if s2.RetryHeld {
		t.Fatal("operator retry did not clear")
	}
	s3 := step(t, s, p, EventDesiredChanged, t0)
	if s3.RetryHeld {
		t.Fatal("desired change did not clear")
	}
	if _, err := Transition(s2, p, EventOperatorRetry, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("retry without hold accepted")
	}
}

func TestRollbackCapNeverOscillates(t *testing.T) {
	p := pol(OnFailureRollback) // cap 1 within 30m
	s := State{Phase: PhaseIdle, Tuple: "t1"}
	s = step(t, s, p, EventStagingStarted, t0)
	s = step(t, s, p, EventActivated, t0)
	s = step(t, s, p, EventUnhealthy, t0)
	if s.Phase != PhaseRollingBack || !s.RetryHeld || len(s.RollbackTimes) != 1 {
		t.Fatalf("%+v", s)
	}
	s = step(t, s, p, EventRollbackActivated, t0)
	if s.Phase != PhaseRolledBack || !s.RetryHeld {
		t.Fatalf("%+v", s)
	}
	// Operator retries the same tuple; it fails again within the window → halted, not another rollback.
	s = step(t, s, p, EventOperatorRetry, t0.Add(time.Minute))
	s = step(t, s, p, EventStagingStarted, t0.Add(time.Minute))
	s = step(t, s, p, EventActivated, t0.Add(time.Minute))
	s = step(t, s, p, EventUnhealthy, t0.Add(2*time.Minute))
	if s.Phase != PhaseHalted || !s.RetryHeld {
		t.Fatalf("expected halted, got %+v", s)
	}
	if _, err := Transition(s, p, EventStagingStarted, t0); err == nil {
		t.Fatal("halted state allowed staging")
	}
	// Outside the window the cap resets.
	late := State{Phase: PhaseActiveGrace, Tuple: "t1", RollbackTimes: []time.Time{t0}}
	n := step(t, late, p, EventUnhealthy, t0.Add(31*time.Minute))
	if n.Phase != PhaseRollingBack {
		t.Fatalf("window did not reset cap: %+v", n)
	}
	// If the rollback realization itself fails, the agent halts rather than trying again.
	h := step(t, n, p, EventRollbackFailed, t0.Add(32*time.Minute))
	if h.Phase != PhaseHalted || !h.RetryHeld {
		t.Fatalf("rollback failure did not halt: %+v", h)
	}
}

func TestLocalFloorCannotBeRelaxedByDesiredState(t *testing.T) {
	p := Policy{OnFailure: OnFailureRollback, MaxAutomaticRollbacks: 10, LocalMaxAutomaticRollbacks: 0}
	if EffectiveRollbackCap(p) != 0 {
		t.Fatal("desired cap exceeded local floor")
	}
	s := State{Phase: PhaseActiveGrace, Tuple: "t1"}
	n := step(t, s, p, EventUnhealthy, t0)
	if n.Phase != PhaseHalted {
		t.Fatalf("rollback happened despite local floor 0: %+v", n)
	}
	p2 := Policy{OnFailure: OnFailureRollback, MaxAutomaticRollbacks: 1, LocalMaxAutomaticRollbacks: 5, LocalRollbackDisabled: true}
	if EffectiveRollbackCap(p2) != 0 {
		t.Fatal("disabled flag ignored")
	}
}

func TestSupervisorRestartInvalidatesAttempt(t *testing.T) {
	p := pol(OnFailureKeep)
	s := State{Phase: PhaseStaging, Tuple: "t1"}
	s = step(t, s, p, EventSupervisorRestart, t0)
	if !s.AttemptInvalidated || s.Phase != PhaseStaging {
		t.Fatalf("%+v", s)
	}
	s = State{Phase: PhaseActiveGrace, Tuple: "t1"}
	s = step(t, s, p, EventSupervisorRestart, t0)
	if !s.AttemptInvalidated {
		t.Fatal("restart during grace not flagged")
	}
	if _, err := Transition(s, p, EventHealthy, t0.Add(time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalidated attempt became healthy: %v", err)
	}
	if _, err := Transition(State{Phase: PhaseStaging}, p, EventDesiredChanged, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("desired change mid-staging accepted without abort")
	}
}

func TestInvalidPolicy(t *testing.T) {
	if _, err := Transition(State{}, Policy{OnFailure: "explode"}, EventHealthy, t0); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatal(err)
	}
}
