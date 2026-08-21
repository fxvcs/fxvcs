PRAGMA user_version = 1;
CREATE TABLE fxvcs_schema_migrations (
  version INTEGER PRIMARY KEY,
  description TEXT NOT NULL,
  checksum TEXT NOT NULL,
  started_at TEXT NOT NULL,
  applied_at TEXT,
  in_progress INTEGER NOT NULL DEFAULT 1 CHECK (in_progress IN (0,1))
);
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
INSERT INTO "fxvcs_schema_migrations" VALUES(1,'publication ledger schema v1: meta, objects, referrers, remotes','sha256:affa9af4a24c50245e69c519aa8346e9a7ae55cee13714fc7c72f654a7acd0a7','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z',0);
INSERT INTO "meta" VALUES('database','publication-ledger');
INSERT INTO "meta" VALUES('schema','1');
INSERT INTO "meta" VALUES('created_by_version','0.1.0-test');
INSERT INTO "meta" VALUES('created_at','2026-01-02T03:04:05Z');
INSERT INTO "meta" VALUES('repository_id','repo_fixture');
INSERT INTO "meta" VALUES('storage_domain_id','domain_fixture');
