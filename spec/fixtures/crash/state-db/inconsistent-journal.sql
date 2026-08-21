-- user_version claims schema 1 but the migration journal records nothing
-- applied. Not resumable: guidance is rebuild for state.db and
-- restore-from-backup for publication.db.
PRAGMA user_version = 1;
CREATE TABLE fxvcs_schema_migrations (
  version INTEGER PRIMARY KEY,
  description TEXT NOT NULL,
  checksum TEXT NOT NULL,
  started_at TEXT NOT NULL,
  applied_at TEXT,
  in_progress INTEGER NOT NULL DEFAULT 1 CHECK (in_progress IN (0,1))
);
