PRAGMA user_version = 2;
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
, ctime_ns INTEGER NOT NULL DEFAULT 0);
CREATE INDEX journal_open ON journal (path) WHERE phase NOT IN ('committed','aborted');
INSERT INTO "fxvcs_schema_migrations" VALUES(1,'hydration state schema v1: meta, paths, journal','sha256:3852e112222db6a8a6055450d5b4256b6cf6e32d0934fc90f8015d8ae28400c6','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z',0);
INSERT INTO "fxvcs_schema_migrations" VALUES(2,'hydration state schema v2: stat cache records change time','sha256:e0f1af3d2b73e85a8914cf40a1cf0432dc93f802e185d200363db40a101690c7','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z',0);
INSERT INTO "journal" VALUES('op-fixture-1',1,'hydrate','assets/hydrated-clean.ytd','committed','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','220:1700000000000000000:1:100',1767323045000000000,1767323045000000000,'ok');
INSERT INTO "journal" VALUES('op-fixture-2',2,'dehydrate','assets/unhydrated.ytd','aborted','sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','0:0:',1767323045000000000,1767323045000000000,'index changed');
INSERT INTO "meta" VALUES('database','hydration-state');
INSERT INTO "meta" VALUES('schema','2');
INSERT INTO "meta" VALUES('created_by_version','0.1.0-test');
INSERT INTO "meta" VALUES('created_at','2026-01-02T03:04:05Z');
INSERT INTO "meta" VALUES('repository_id','repo_fixture');
INSERT INTO "meta" VALUES('storage_domain_id','domain_fixture');
INSERT INTO "paths" VALUES('assets/hydrated-clean.ytd','hydrated','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',4096,1700000000000000000,'1:100',1700000001000000000,1,0);
INSERT INTO "paths" VALUES('assets/unhydrated.ytd','unhydrated','sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',220,1700000000000000000,'1:101',0,0,0);
