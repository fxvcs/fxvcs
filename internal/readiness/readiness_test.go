package readiness

import (
	"context"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestThresholdsGraceAndSoak(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	policy := Policy{StartupGrace: 10 * time.Second, SuccessThreshold: 2, FailureThreshold: 2, SoakDuration: 5 * time.Second}
	state := Tracker{ActivatedAt: t0, Decision: Pending}
	state, _ = Observe(state, policy, t0.Add(time.Second), false)
	if state.ConsecutiveFailures != 0 || state.Decision != Pending {
		t.Fatalf("grace failure counted: %#v", state)
	}
	state, _ = Observe(state, policy, t0.Add(11*time.Second), true)
	state, _ = Observe(state, policy, t0.Add(12*time.Second), true)
	if state.Decision != Pending || state.HealthySince.IsZero() {
		t.Fatalf("soak skipped: %#v", state)
	}
	state, _ = Observe(state, policy, t0.Add(17*time.Second), true)
	if state.Decision != Healthy {
		t.Fatalf("healthy not reached: %#v", state)
	}
}

func TestFailureThresholdAfterGrace(t *testing.T) {
	t0 := time.Now().UTC()
	policy := Policy{SuccessThreshold: 1, FailureThreshold: 2}
	state := Tracker{ActivatedAt: t0, Decision: Pending}
	state, _ = Observe(state, policy, t0.Add(time.Second), false)
	state, _ = Observe(state, policy, t0.Add(2*time.Second), false)
	if state.Decision != Unhealthy {
		t.Fatalf("unhealthy not reached: %#v", state)
	}
}

type staticChecker struct{ evidence Evidence }

func (s staticChecker) Check(context.Context, domain.HealthCheck) Evidence { return s.evidence }

func TestRunnerPreservesDeclaredOrderAndFailsUnknown(t *testing.T) {
	runner := Runner{Checkers: map[string]Checker{
		"tcp": staticChecker{Evidence{Interface: "tcp:30120", Success: true}},
	}}
	results := runner.Run(context.Background(), []domain.HealthCheck{{Type: "tcp"}, {Type: "rcon-status"}}, time.Second)
	if len(results) != 2 || results[0].Type != "tcp" || !results[0].Success || results[1].Success {
		t.Fatalf("unexpected evidence: %#v", results)
	}
	if AllPassed(results) {
		t.Fatal("failed check batch reported healthy")
	}
}
