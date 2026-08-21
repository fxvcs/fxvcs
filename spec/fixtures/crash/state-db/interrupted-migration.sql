-- Process died during the 0->1 migration: the in-progress marker was durably
-- written, the schema transaction rolled back, user_version is still 0.
-- Verify reports ErrInterruptedMigration; Status.Resumable() is true; opening
-- with Migrate re-runs the step idempotently.
PRAGMA user_version = 0;
CREATE TABLE fxvcs_schema_migrations (
  version INTEGER PRIMARY KEY,
  description TEXT NOT NULL,
  checksum TEXT NOT NULL,
  started_at TEXT NOT NULL,
  applied_at TEXT,
  in_progress INTEGER NOT NULL DEFAULT 1 CHECK (in_progress IN (0,1))
);
INSERT INTO "fxvcs_schema_migrations" VALUES(1,'hydration state schema v1: meta, paths, journal','sha256:3852e112222db6a8a6055450d5b4256b6cf6e32d0934fc90f8015d8ae28400c6','2026-01-02T03:04:05Z',NULL,1);
