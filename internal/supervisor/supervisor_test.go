package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRunner struct {
	name string
	args []string
	out  string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name, r.args = name, append([]string(nil), args...)
	return []byte(r.out), nil
}

func TestSystemdUsesTypedArgvAndProcessGeneration(t *testing.T) {
	runner := &fakeRunner{out: "ActiveState=active\nMainPID=42\nExecMainStartTimestampMonotonic=9988\n"}
	adapter := Systemd{Unit: "fxserver.service", Runner: runner}
	observation, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Running || observation.ProcessGeneration != "systemd:42:9988" {
		t.Fatalf("unexpected observation: %#v", observation)
	}
	if runner.name != "systemctl" || strings.Join(runner.args, " ") != "show --no-pager --property=ActiveState,MainPID,ExecMainStartTimestampMonotonic fxserver.service" {
		t.Fatalf("unexpected argv: %s %#v", runner.name, runner.args)
	}
	if err := adapter.Restart(context.Background()); err != nil || strings.Join(runner.args, " ") != "restart fxserver.service" {
		t.Fatalf("restart: %#v %v", runner.args, err)
	}
}

func TestManualNeverClaimsControl(t *testing.T) {
	manual := Manual{}
	if manual.Capabilities().Restart {
		t.Fatal("manual supervisor claimed restart")
	}
	if _, err := manual.Observe(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("manual observation: %v", err)
	}
}

type fakeTxAdmin struct{}

func (fakeTxAdmin) Observe(context.Context) (Observation, error)    { return Observation{}, nil }
func (fakeTxAdmin) Restart(context.Context) error                   { return nil }
func (fakeTxAdmin) ReloadResources(context.Context, []string) error { return nil }
func (fakeTxAdmin) Capabilities() Capabilities                      { return Capabilities{Observe: true} }

func TestTxAdminCannotBypassLiveLabGate(t *testing.T) {
	if _, err := NewTxAdmin(TxAdminEvidence{Passed: map[string]bool{"E1": true}}, fakeTxAdmin{}); !errors.Is(err, ErrLabGate) {
		t.Fatalf("incomplete evidence accepted: %v", err)
	}
	passed := map[string]bool{}
	for _, experiment := range RequiredTxAdminExperiments {
		passed[experiment] = true
	}
	if _, err := NewTxAdmin(TxAdminEvidence{Passed: passed}, fakeTxAdmin{}); err != nil {
		t.Fatalf("complete evidence refused: %v", err)
	}
}
