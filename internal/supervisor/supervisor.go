// Package supervisor defines the capability boundary between reconciliation
// and target-provided process supervisors. Capabilities are explicit; an
// adapter never infers safe control from observation alone.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrUnsupported = errors.New("supervisor: capability unsupported")
	ErrLabGate     = errors.New("supervisor: txadmin lab evidence incomplete")
)

type Capabilities struct {
	Observe         bool
	Restart         bool
	HotReload       bool
	PlayerCount     bool
	ProcessIdentity bool
}

type Observation struct {
	Running           bool
	ProcessGeneration string
	PlayerCount       *int
	SupervisorVersion string
	EvidenceSource    string
}

type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Observe(context.Context) (Observation, error)
	Restart(context.Context) error
	ReloadResources(context.Context, []string) error
}

// Manual is observe-only when ObserveFunc is supplied and otherwise has no
// capabilities. It never pretends an operator-managed process can be safely
// restarted by FXVCS.
type Manual struct {
	ObserveFunc func(context.Context) (Observation, error)
}

func (Manual) Name() string { return "manual" }
func (m Manual) Capabilities() Capabilities {
	return Capabilities{Observe: m.ObserveFunc != nil, ProcessIdentity: m.ObserveFunc != nil}
}
func (m Manual) Observe(ctx context.Context) (Observation, error) {
	if m.ObserveFunc == nil {
		return Observation{}, ErrUnsupported
	}
	return m.ObserveFunc(ctx)
}
func (Manual) Restart(context.Context) error                   { return ErrUnsupported }
func (Manual) ReloadResources(context.Context, []string) error { return ErrUnsupported }

// Runner is an argv-based process boundary; implementations must not invoke a
// shell. Tests and SDK hosts can inject it without parsing human CLI output in
// the reconciliation engine.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Systemd struct {
	Unit   string
	Runner Runner
}

func (Systemd) Name() string { return "systemd" }
func (Systemd) Capabilities() Capabilities {
	return Capabilities{Observe: true, Restart: true, ProcessIdentity: true}
}
func (s Systemd) Observe(ctx context.Context) (Observation, error) {
	if s.Runner == nil || s.Unit == "" {
		return Observation{}, errors.New("supervisor: incomplete systemd binding")
	}
	out, err := s.Runner.Run(ctx, "systemctl", "show", "--no-pager", "--property=ActiveState,MainPID,ExecMainStartTimestampMonotonic", s.Unit)
	if err != nil {
		return Observation{}, err
	}
	values := parseProperties(string(out))
	pid, err := strconv.ParseUint(values["MainPID"], 10, 64)
	if err != nil {
		return Observation{}, fmt.Errorf("supervisor: invalid systemd MainPID: %w", err)
	}
	started := values["ExecMainStartTimestampMonotonic"]
	if started == "" {
		return Observation{}, errors.New("supervisor: systemd omitted process start identity")
	}
	return Observation{
		Running:           values["ActiveState"] == "active" && pid != 0,
		ProcessGeneration: fmt.Sprintf("systemd:%d:%s", pid, started),
		EvidenceSource:    "systemd-show",
	}, nil
}
func (s Systemd) Restart(ctx context.Context) error {
	if s.Runner == nil || s.Unit == "" {
		return errors.New("supervisor: incomplete systemd binding")
	}
	_, err := s.Runner.Run(ctx, "systemctl", "restart", s.Unit)
	return err
}
func (Systemd) ReloadResources(context.Context, []string) error { return ErrUnsupported }

func parseProperties(output string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	return values
}

// RequiredTxAdminExperiments are the binary live-lab gates from the Stage 4
// compatibility matrix. An adapter cannot be constructed until every enabled
// mechanism has affirmative evidence.
var RequiredTxAdminExperiments = []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8", "E9", "E10", "E11", "E13", "E14"}

type TxAdminEvidence struct {
	Passed map[string]bool
}

func (e TxAdminEvidence) Missing() []string {
	var missing []string
	for _, experiment := range RequiredTxAdminExperiments {
		if !e.Passed[experiment] {
			missing = append(missing, experiment)
		}
	}
	sort.Strings(missing)
	return missing
}

// TxAdminTransport is intentionally typed rather than an HTTP/browser API.
// The eventual implementation must be backed only by mechanisms proven in the
// lab matrix; browser sessions and CSRF automation are outside this boundary.
type TxAdminTransport interface {
	Observe(context.Context) (Observation, error)
	Restart(context.Context) error
	ReloadResources(context.Context, []string) error
	Capabilities() Capabilities
}

type TxAdmin struct{ transport TxAdminTransport }

func NewTxAdmin(evidence TxAdminEvidence, transport TxAdminTransport) (*TxAdmin, error) {
	if missing := evidence.Missing(); len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %s", ErrLabGate, strings.Join(missing, ","))
	}
	if transport == nil {
		return nil, errors.New("supervisor: txadmin transport required")
	}
	return &TxAdmin{transport: transport}, nil
}

func (*TxAdmin) Name() string                                       { return "txadmin" }
func (t *TxAdmin) Capabilities() Capabilities                       { return t.transport.Capabilities() }
func (t *TxAdmin) Observe(ctx context.Context) (Observation, error) { return t.transport.Observe(ctx) }
func (t *TxAdmin) Restart(ctx context.Context) error                { return t.transport.Restart(ctx) }
func (t *TxAdmin) ReloadResources(ctx context.Context, names []string) error {
	return t.transport.ReloadResources(ctx, append([]string(nil), names...))
}
