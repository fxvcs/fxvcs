-- state.db whose process died after fetching bytes for a hydrate operation
-- (journal phase "fetched", never "committed"). Reopening lists it as
-- interrupted; reconciliation decides keep-dirty/index-moved/safe-rebuild.
PRAGMA user_version = 1;
CREATE TABLE fxvcs_schema_migrations (
  version INTEGER PRIMARY KEY,
  description TEXT NOT NULL,
  checksum TEXT NOT NULL,
  started_at TEXT NOT NULL,
  applied_at TEXT,
  in_progress INTEGER NOT NULL DEFAULT 1 CHECK (in_progress IN (0,1))
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
INSERT INTO "fxvcs_schema_migrations" VALUES(1,'hydration state schema v1: meta, paths, journal','sha256:3852e112222db6a8a6055450d5b4256b6cf6e32d0934fc90f8015d8ae28400c6','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z',0);
INSERT INTO "meta" VALUES('database','hydration-state');
INSERT INTO "meta" VALUES('schema','1');
INSERT INTO "meta" VALUES('created_by_version','0.1.0-test');
INSERT INTO "meta" VALUES('created_at','2026-01-02T03:04:05Z');
INSERT INTO "paths" VALUES('assets/crashed.ytd','unhydrated','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',220,1700000000000000000,'1:100',0,0);
INSERT INTO "journal" VALUES('op-crashed-1',1,'hydrate','assets/crashed.ytd','fetched','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','220:1700000000000000000:1:100',1767323045000000000,0,'');
