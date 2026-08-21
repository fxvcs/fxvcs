-- Empty database: PRAGMA user_version 0, no tables. Migrating it produces the
-- current schema (see expected-v0-to-current.sql).
PRAGMA user_version = 0;
