-- Written by a hypothetical future binary. Opening must fail with
-- migrate.ErrNewerThanSupported before any write.
PRAGMA user_version = 999;
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE fxvcs_schema_migrations (version INTEGER PRIMARY KEY, description TEXT NOT NULL, checksum TEXT NOT NULL, started_at TEXT NOT NULL, applied_at TEXT, in_progress INTEGER NOT NULL DEFAULT 1);
INSERT INTO fxvcs_schema_migrations VALUES(999,'future schema','sha256:0','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z',0);
INSERT INTO meta VALUES('database','hydration-state');
INSERT INTO meta VALUES('schema','999');
