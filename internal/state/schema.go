package state

import "fxvcs.dev/fxvcs/internal/migrate"

// Schema DDL, version 1. The text is normative and mirrored in
// docs/spec/local-state.md; changing it requires a new migration step, never an
// edit of an existing one (the journal records each step's checksum).
const (
	hydrationDDLv1 = `
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE paths (
  path TEXT PRIMARY KEY,
  intent TEXT NOT NULL CHECK (intent IN ('unhydrated','hydrated')),
  index_pointer_digest TEXT NOT NULL,
  working_digest TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  mtime_ns INTEGER NOT NULL DEFAULT 0,
  file_id TEXT NOT NULL DEFAULT '',
  last_verified_ns INTEGER NOT NULL DEFAULT 0,
  generation INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE journal (
  op_id TEXT PRIMARY KEY,
  generation INTEGER NOT NULL,
  operation TEXT NOT NULL,
  path TEXT NOT NULL,
  phase TEXT NOT NULL CHECK (phase IN ('started','fetched','verified','pinned','committed','aborted')),
  expected_index_oid TEXT NOT NULL DEFAULT '',
  expected_fingerprint TEXT NOT NULL DEFAULT '',
  started_ns INTEGER NOT NULL,
  finished_ns INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX journal_open ON journal (path) WHERE phase NOT IN ('committed','aborted');
`

	publicationDDLv1 = `
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE objects (
  digest TEXT PRIMARY KEY,
  storage_domain_id TEXT NOT NULL,
  size INTEGER NOT NULL,
  encoding TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  ingested_ns INTEGER NOT NULL,
  pending INTEGER NOT NULL DEFAULT 1 CHECK (pending IN (0,1))
);
CREATE TABLE referrers (
  digest TEXT NOT NULL REFERENCES objects(digest) ON DELETE CASCADE,
  path TEXT NOT NULL,
  commit_or_index TEXT NOT NULL,
  PRIMARY KEY (digest, path, commit_or_index)
);
CREATE TABLE remotes (
  digest TEXT NOT NULL REFERENCES objects(digest) ON DELETE CASCADE,
  remote_name TEXT NOT NULL,
  published_ns INTEGER NOT NULL,
  PRIMARY KEY (digest, remote_name)
);
CREATE INDEX objects_pending ON objects (pending) WHERE pending = 1;
`

	agentDDLv1 = `
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE deployments (
  realization_id TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  inputs_json BLOB NOT NULL,
  phase TEXT NOT NULL CHECK (phase IN ('idle','staging','active-grace','active-healthy','active-unhealthy','rolling-back','rolled-back','activation-failed','halted')),
  retry_held INTEGER NOT NULL DEFAULT 0 CHECK (retry_held IN (0,1)),
  attempt_invalidated INTEGER NOT NULL DEFAULT 0 CHECK (attempt_invalidated IN (0,1)),
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT '',
  observed_runtime_json BLOB,
  process_generation TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX deployments_started ON deployments (started_at DESC, realization_id);
CREATE TABLE target_state (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  current_realization_id TEXT NOT NULL DEFAULT '',
  last_known_good_realization_id TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO target_state(singleton) VALUES (1);
CREATE TABLE health_samples (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  realization_id TEXT NOT NULL REFERENCES deployments(realization_id) ON DELETE CASCADE,
  check_type TEXT NOT NULL,
  interface TEXT NOT NULL,
  success INTEGER NOT NULL CHECK (success IN (0,1)),
  observed_at TEXT NOT NULL,
  latency_ns INTEGER NOT NULL DEFAULT 0 CHECK (latency_ns >= 0),
  detail_json BLOB
);
CREATE INDEX health_samples_realization ON health_samples (realization_id, id);
CREATE TABLE rollback_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  realization_id TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX rollback_events_time ON rollback_events (occurred_at, realization_id);
CREATE TABLE operation_journal (
  op_id TEXT PRIMARY KEY,
  operation TEXT NOT NULL,
  realization_id TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL,
  payload_json BLOB,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX operation_journal_open ON operation_journal (updated_at) WHERE outcome = '';
CREATE TABLE prune_plans (
  plan_id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('planned','applying','applied','invalidated','failed')),
  candidates_json BLOB NOT NULL,
  applied_at TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT ''
);
CREATE TABLE retained_realizations (
  realization_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  retained_at TEXT NOT NULL
);
CREATE TABLE prune_deletions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_id TEXT NOT NULL UNIQUE,
  realization_id TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);
CREATE INDEX prune_deletions_time ON prune_deletions (deleted_at, realization_id);
`
)

func ddl(sqlText string) func(tx migrate.Tx) error {
	return func(tx migrate.Tx) error {
		_, err := tx.Exec(sqlText)
		return err
	}
}

// hydrationDDLv2 adds the change-time column to the stat cache. Existing rows
// get zero, which differs from any real file's change time and so reads as
// "changed": one re-hash per path after the upgrade, and no wrong answers.
const hydrationDDLv2 = `
ALTER TABLE paths ADD COLUMN ctime_ns INTEGER NOT NULL DEFAULT 0;
`

// HydrationMigrations is the ordered chain for state.db; Latest() equals
// format.HydrationStateSchema.
func HydrationMigrations() *migrate.Registry {
	return migrate.MustRegistry(
		migrate.Migration{From: 0, To: 1, Description: "hydration state schema v1: meta, paths, journal", Apply: ddl(hydrationDDLv1)},
		migrate.Migration{From: 1, To: 2, Description: "hydration state schema v2: stat cache records change time", Apply: ddl(hydrationDDLv2)},
	)
}

// PublicationMigrations is the ordered chain for publication.db; Latest()
// equals format.PublicationLedgerSchema.
func PublicationMigrations() *migrate.Registry {
	return migrate.MustRegistry(
		migrate.Migration{From: 0, To: 1, Description: "publication ledger schema v1: meta, objects, referrers, remotes", Apply: ddl(publicationDDLv1)},
	)
}

// AgentMigrations is the ordered chain for <deployRoot>/state/agent.db;
// Latest() equals format.AgentStateSchema.
func AgentMigrations() *migrate.Registry {
	return migrate.MustRegistry(
		migrate.Migration{From: 0, To: 1, Description: "agent state schema v1: deployments, target state, health, journals, and prune plans", Apply: ddl(agentDDLv1)},
	)
}
