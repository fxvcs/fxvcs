package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/migrate"
)

// AgentDB is one target's <deployRoot>/state/agent.db. It records evidence and
// recovery intent, but never overrides the filesystem current link as the
// authority for which realization is active.
type AgentDB struct{ *handle }

// OpenAgent opens or creates a per-target agent state database. opts.Target is
// required and is permanently bound into database metadata.
func OpenAgent(ctx context.Context, path string, opts Options) (*AgentDB, error) {
	if opts.Target == "" {
		return nil, errors.New("state: OpenAgent requires Options.Target")
	}
	h, err := open(ctx, path, KindAgentState, AgentMigrations(), opts)
	if err != nil {
		return nil, err
	}
	return &AgentDB{h}, nil
}

func validDeploymentPhase(phase string) bool {
	switch phase {
	case "idle", "staging", "active-grace", "active-healthy", "active-unhealthy", "rolling-back", "rolled-back", "activation-failed", "halted":
		return true
	default:
		return false
	}
}

func validTimestamp(value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("state: invalid RFC3339 timestamp %q: %w", value, err)
	}
	return nil
}

// PutDeployment atomically inserts or updates one realization's durable
// deployment record. JSON blobs use the canonical encoder so database dumps
// and operation evidence remain deterministic.
func (d *AgentDB) PutDeployment(ctx context.Context, dep domain.Deployment) error {
	if _, err := layout.Realization("/deploy", dep.RealizationID); err != nil {
		return err
	}
	if dep.Target == "" || dep.Target != d.opts.Target {
		return fmt.Errorf("%w: deployment target %q, database target %q", ErrWrongTarget, dep.Target, d.opts.Target)
	}
	if !validDeploymentPhase(dep.Phase) {
		return fmt.Errorf("state: invalid deployment phase %q", dep.Phase)
	}
	if err := validTimestamp(dep.StartedAt, false); err != nil {
		return err
	}
	if err := validTimestamp(dep.FinishedAt, true); err != nil {
		return err
	}
	inputs, err := canonical.Marshal(dep.Inputs)
	if err != nil {
		return err
	}
	var observed []byte
	if dep.ObservedRuntime != nil {
		observed, err = canonical.Marshal(dep.ObservedRuntime)
		if err != nil {
			return err
		}
	}
	held := 0
	if dep.RetryHeld {
		held = 1
	}
	invalidated := 0
	if dep.AttemptInvalidated {
		invalidated = 1
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO deployments
			(realization_id,target,inputs_json,phase,retry_held,attempt_invalidated,started_at,finished_at,observed_runtime_json,process_generation,outcome)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(realization_id) DO UPDATE SET
			inputs_json=excluded.inputs_json, phase=excluded.phase, retry_held=excluded.retry_held, attempt_invalidated=excluded.attempt_invalidated,
			started_at=excluded.started_at, finished_at=excluded.finished_at,
			observed_runtime_json=excluded.observed_runtime_json,
			process_generation=excluded.process_generation, outcome=excluded.outcome`,
			dep.RealizationID, dep.Target, inputs, dep.Phase, held, invalidated, dep.StartedAt, dep.FinishedAt,
			nullBlob(observed), dep.ProcessGeneration, dep.Outcome)
		return err
	})
}

func nullBlob(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

const deploymentColumns = `realization_id,target,inputs_json,phase,retry_held,attempt_invalidated,started_at,finished_at,observed_runtime_json,process_generation,outcome`

func scanDeployment(row interface{ Scan(...any) error }) (domain.Deployment, error) {
	var dep domain.Deployment
	var inputs []byte
	var observed []byte
	var held, invalidated int
	if err := row.Scan(&dep.RealizationID, &dep.Target, &inputs, &dep.Phase, &held, &invalidated, &dep.StartedAt,
		&dep.FinishedAt, &observed, &dep.ProcessGeneration, &dep.Outcome); err != nil {
		return dep, err
	}
	dep.RetryHeld = held == 1
	dep.AttemptInvalidated = invalidated == 1
	if err := json.Unmarshal(inputs, &dep.Inputs); err != nil {
		return dep, fmt.Errorf("state: decode realization inputs: %w", err)
	}
	if len(observed) > 0 {
		dep.ObservedRuntime = new(domain.ObservedRuntime)
		if err := json.Unmarshal(observed, dep.ObservedRuntime); err != nil {
			return dep, fmt.Errorf("state: decode observed runtime: %w", err)
		}
	}
	return dep, nil
}

// Deployment returns one realization record.
func (d *AgentDB) Deployment(ctx context.Context, realizationID string) (domain.Deployment, bool, error) {
	dep, err := scanDeployment(d.db.QueryRowContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE realization_id = ?", realizationID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Deployment{}, false, nil
	}
	return dep, err == nil, err
}

// Deployments lists newest attempts first with realization ID as the stable
// tie breaker.
func (d *AgentDB) Deployments(ctx context.Context) ([]domain.Deployment, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+deploymentColumns+" FROM deployments ORDER BY started_at DESC, realization_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

// TargetState is the database's last observation of filesystem activation.
// CurrentRealizationID must always be re-observed from current before acting.
type TargetState struct {
	CurrentRealizationID       string
	LastKnownGoodRealizationID string
	ObservedAt                 string
}

// ObserveTargetState replaces the cached filesystem observation and LKG in
// one transaction. IDs may be empty; non-empty values must be valid.
func (d *AgentDB) ObserveTargetState(ctx context.Context, state TargetState) error {
	for _, id := range []string{state.CurrentRealizationID, state.LastKnownGoodRealizationID} {
		if id != "" {
			if _, err := layout.Realization("/deploy", id); err != nil {
				return err
			}
		}
	}
	if err := validTimestamp(state.ObservedAt, false); err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`UPDATE target_state SET current_realization_id=?, last_known_good_realization_id=?, observed_at=? WHERE singleton=1`,
			state.CurrentRealizationID, state.LastKnownGoodRealizationID, state.ObservedAt)
		return err
	})
}

// TargetState returns the cached observation. Callers must not use it as
// activation authority without comparing it to the filesystem current link.
func (d *AgentDB) TargetState(ctx context.Context) (TargetState, error) {
	var state TargetState
	err := d.db.QueryRowContext(ctx, `SELECT current_realization_id,last_known_good_realization_id,observed_at FROM target_state WHERE singleton=1`).
		Scan(&state.CurrentRealizationID, &state.LastKnownGoodRealizationID, &state.ObservedAt)
	return state, err
}

// HealthSample is independently bounded readiness evidence and names the
// interface through which the observation was made.
type HealthSample struct {
	ID            int64
	RealizationID string
	CheckType     string
	Interface     string
	Success       bool
	ObservedAt    string
	LatencyNS     int64
	Detail        json.RawMessage
}

func (d *AgentDB) RecordHealthSample(ctx context.Context, sample HealthSample) error {
	if sample.CheckType == "" || sample.Interface == "" || sample.LatencyNS < 0 {
		return errors.New("state: incomplete health sample")
	}
	if err := validTimestamp(sample.ObservedAt, false); err != nil {
		return err
	}
	success := 0
	if sample.Success {
		success = 1
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO health_samples(realization_id,check_type,interface,success,observed_at,latency_ns,detail_json) VALUES(?,?,?,?,?,?,?)`,
			sample.RealizationID, sample.CheckType, sample.Interface, success, sample.ObservedAt, sample.LatencyNS, nullBlob(sample.Detail))
		return err
	})
}

func (d *AgentDB) HealthSamples(ctx context.Context, realizationID string) ([]HealthSample, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id,realization_id,check_type,interface,success,observed_at,latency_ns,detail_json FROM health_samples WHERE realization_id=? ORDER BY id`, realizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthSample
	for rows.Next() {
		var sample HealthSample
		var success int
		var detail []byte
		if err := rows.Scan(&sample.ID, &sample.RealizationID, &sample.CheckType, &sample.Interface, &success, &sample.ObservedAt, &sample.LatencyNS, &detail); err != nil {
			return nil, err
		}
		sample.Success = success == 1
		sample.Detail = detail
		out = append(out, sample)
	}
	return out, rows.Err()
}

// RecordRollback appends durable automatic-rollback cap evidence.
func (d *AgentDB) RecordRollback(ctx context.Context, realizationID string, occurredAt time.Time) error {
	if _, err := layout.Realization("/deploy", realizationID); err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO rollback_events(realization_id,occurred_at) VALUES(?,?)`, realizationID, occurredAt.UTC().Format(time.RFC3339Nano))
		return err
	})
}

// RollbackTimes returns rollback events at or after since, oldest first.
func (d *AgentDB) RollbackTimes(ctx context.Context, since time.Time) ([]time.Time, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT occurred_at FROM rollback_events WHERE occurred_at>=? ORDER BY occurred_at,id`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, rows.Err()
}

// OperationRecord is a crash-recovery journal row. Outcome is empty while an
// operation is open; phase and payload describe the last durable boundary.
type OperationRecord struct {
	OperationID   string
	Operation     string
	RealizationID string
	Phase         string
	Payload       json.RawMessage
	StartedAt     string
	UpdatedAt     string
	Outcome       string
}

// BeginOperation creates a journal before the first filesystem mutation.
func (d *AgentDB) BeginOperation(ctx context.Context, rec OperationRecord) error {
	if rec.OperationID == "" || rec.Operation == "" || rec.Phase == "" || rec.Outcome != "" {
		return errors.New("state: incomplete open operation record")
	}
	if rec.RealizationID != "" {
		if _, err := layout.Realization("/deploy", rec.RealizationID); err != nil {
			return err
		}
	}
	now := d.opts.now().UTC().Format(time.RFC3339Nano)
	if rec.StartedAt == "" {
		rec.StartedAt = now
	}
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = rec.StartedAt
	}
	if err := validTimestamp(rec.StartedAt, false); err != nil {
		return err
	}
	if err := validTimestamp(rec.UpdatedAt, false); err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO operation_journal(op_id,operation,realization_id,phase,payload_json,started_at,updated_at,outcome) VALUES(?,?,?,?,?,?,?,?)`,
			rec.OperationID, rec.Operation, rec.RealizationID, rec.Phase, nullBlob(rec.Payload), rec.StartedAt, rec.UpdatedAt, "")
		return err
	})
}

// BeginOrReopenPruneOperation starts a deterministic prune candidate journal,
// or reopens the same exact closed candidate after recovery aborted before the
// rename. A different operation, realization, or payload can never reuse the
// ID.
func (d *AgentDB) BeginOrReopenPruneOperation(ctx context.Context, rec OperationRecord) error {
	if rec.Operation != "prune" || rec.OperationID == "" || rec.RealizationID == "" || rec.Phase == "" || rec.Outcome != "" || len(rec.Payload) == 0 {
		return errors.New("state: incomplete prune operation record")
	}
	if _, err := layout.Realization("/deploy", rec.RealizationID); err != nil {
		return err
	}
	now := d.opts.now().UTC().Format(time.RFC3339Nano)
	return d.tx(ctx, func(tx migrate.Tx) error {
		result, err := tx.Exec(`INSERT INTO operation_journal(op_id,operation,realization_id,phase,payload_json,started_at,updated_at,outcome)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(op_id) DO UPDATE SET phase=excluded.phase,started_at=excluded.started_at,updated_at=excluded.updated_at,outcome=''
			WHERE operation_journal.operation='prune' AND operation_journal.realization_id=excluded.realization_id
			AND operation_journal.payload_json=excluded.payload_json AND operation_journal.outcome!=''`,
			rec.OperationID, rec.Operation, rec.RealizationID, rec.Phase, []byte(rec.Payload), now, now, "")
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("state: prune operation %q conflicts with different or open evidence", rec.OperationID)
		}
		return nil
	})
}

// AdvanceOperation durably records a completed boundary. A non-empty outcome
// closes the operation; closed operations cannot be advanced again.
func (d *AgentDB) AdvanceOperation(ctx context.Context, operationID, phase string, payload json.RawMessage, outcome string) error {
	if operationID == "" || phase == "" {
		return errors.New("state: operation ID and phase required")
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		result, err := tx.Exec(`UPDATE operation_journal SET phase=?,payload_json=?,updated_at=?,outcome=? WHERE op_id=? AND outcome=''`,
			phase, nullBlob(payload), d.opts.now().UTC().Format(time.RFC3339Nano), outcome, operationID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("state: operation %q is absent or already closed", operationID)
		}
		return nil
	})
}

func scanOperation(row interface{ Scan(...any) error }) (OperationRecord, error) {
	var rec OperationRecord
	var payload []byte
	err := row.Scan(&rec.OperationID, &rec.Operation, &rec.RealizationID, &rec.Phase, &payload, &rec.StartedAt, &rec.UpdatedAt, &rec.Outcome)
	rec.Payload = payload
	return rec, err
}

// OpenOperations returns unfinished recovery records oldest first.
func (d *AgentDB) OpenOperations(ctx context.Context) ([]OperationRecord, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT op_id,operation,realization_id,phase,payload_json,started_at,updated_at,outcome FROM operation_journal WHERE outcome='' ORDER BY started_at,op_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationRecord
	for rows.Next() {
		rec, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// PrunePlanRecord persists the exact candidate set produced by a dry run.
// Candidates must be canonical JSON generated by the pruning planner.
type PrunePlanRecord struct {
	PlanID     string
	CreatedAt  string
	Status     string
	Candidates json.RawMessage
	AppliedAt  string
	Outcome    string
}

func validPruneStatus(status string) bool {
	switch status {
	case "planned", "applying", "applied", "invalidated", "failed":
		return true
	default:
		return false
	}
}

func (d *AgentDB) PutPrunePlan(ctx context.Context, plan PrunePlanRecord) error {
	if plan.PlanID == "" || !validPruneStatus(plan.Status) || len(plan.Candidates) == 0 {
		return errors.New("state: incomplete prune plan")
	}
	if err := validTimestamp(plan.CreatedAt, false); err != nil {
		return err
	}
	if err := validTimestamp(plan.AppliedAt, true); err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO prune_plans(plan_id,created_at,status,candidates_json,applied_at,outcome) VALUES(?,?,?,?,?,?)
			ON CONFLICT(plan_id) DO UPDATE SET status=excluded.status,applied_at=excluded.applied_at,outcome=excluded.outcome`,
			plan.PlanID, plan.CreatedAt, plan.Status, []byte(plan.Candidates), plan.AppliedAt, plan.Outcome)
		return err
	})
}

func (d *AgentDB) PrunePlan(ctx context.Context, planID string) (PrunePlanRecord, bool, error) {
	var plan PrunePlanRecord
	var candidates []byte
	err := d.db.QueryRowContext(ctx, `SELECT plan_id,created_at,status,candidates_json,applied_at,outcome FROM prune_plans WHERE plan_id=?`, planID).
		Scan(&plan.PlanID, &plan.CreatedAt, &plan.Status, &candidates, &plan.AppliedAt, &plan.Outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return PrunePlanRecord{}, false, nil
	}
	plan.Candidates = candidates
	return plan, err == nil, err
}

// RetainRealization marks a realization as operator-protected from pruning.
func (d *AgentDB) RetainRealization(ctx context.Context, realizationID, reason string) error {
	if _, err := layout.Realization("/deploy", realizationID); err != nil {
		return err
	}
	if reason == "" {
		return errors.New("state: retention reason required")
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO retained_realizations(realization_id,reason,retained_at) VALUES(?,?,?)
			ON CONFLICT(realization_id) DO UPDATE SET reason=excluded.reason`, realizationID, reason, d.opts.now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (d *AgentDB) RetainedRealizations(ctx context.Context) (map[string]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT realization_id,reason FROM retained_realizations ORDER BY realization_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		out[id] = reason
	}
	return out, rows.Err()
}

func (d *AgentDB) RecordPruneDeletion(ctx context.Context, operationID, realizationID string, deletedAt time.Time) error {
	if operationID == "" {
		return errors.New("state: prune deletion operation ID required")
	}
	if _, err := layout.Realization("/deploy", realizationID); err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO prune_deletions(operation_id,realization_id,deleted_at) VALUES(?,?,?) ON CONFLICT(operation_id) DO NOTHING`, operationID, realizationID, deletedAt.UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (d *AgentDB) PruneDeletionTimes(ctx context.Context, since time.Time) ([]time.Time, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT deleted_at FROM prune_deletions WHERE deleted_at>=? ORDER BY deleted_at,id`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, rows.Err()
}

func (d *AgentDB) HasPruneDeletion(ctx context.Context, operationID string) (bool, error) {
	var one int
	err := d.db.QueryRowContext(ctx, `SELECT 1 FROM prune_deletions WHERE operation_id=?`, operationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
