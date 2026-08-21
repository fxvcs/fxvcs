# Runtime Layout and Target Runtime Contract

Status: planning draft

## 1. Filesystem layout

For a Linux txAdmin or vanilla installation, the logical layout is:

```text
<serverData>/
  server.cfg                         stable bootstrap: exec fxvcs.cfg
  fxvcs.cfg                          -> <deployRoot>/current/runtime/fxvcs.cfg
  resources                         -> <deployRoot>/current/resources
  cache                             -> <deployRoot>/shared/cache

<deployRoot>/
  current                            -> releases/<realizationID>
  releases/<realizationID>/
    resources/
    runtime/fxvcs.cfg
    runtime/config/
  materialized/                      verified digest-keyed ordinary files
  shared/
    cache/
    persistent/<repositoryID>/<resource-path>/
  state/

<txData>/                            txAdmin profiles, logs, database, state
```

`txData` remains outside realizations and is never exposed through the resource
mount. Windows uses equivalent atomic directory indirection with junctions,
symlinks, or a launcher-managed path selected by platform capability tests.
Accepted physical bindings are agent-local; portable targets contain only
logical references.

The diagnostic FTP/SFTP root may expose `<serverData>/resources`, but must not
expose `<deployRoot>/shared`, secret-bearing runtime config, agent state, or
`txData` unless the host administrator deliberately grants separate access.

## 2. Mutable resource state

Realization and materialized files are read-only. A component that writes a
database, generated file, uploads, or other durable state declares normalized
`persistentPaths` in its resource catalog. During staging, FXVCS binds those
paths to the resource's target-scoped directory under `shared/persistent`.
Every entry also has an exact `persistentPathKinds` value of `file` or
`directory`; FXVCS never infers kind from a filename extension. V1 creates an
empty target for runtime-owned data and refuses source-controlled bytes at or
beneath a persistent path rather than silently seeding or discarding them.

Persistent paths must be relative, non-overlapping, symlink-safe, and contained
within the resource. Persistent data is excluded from Release identity, rollback,
hard-link deduplication, and realization pruning; backup and migration remain
explicit operator responsibilities.

For adoption compatibility, a resource may explicitly set
`mutableMode: copy` instead of the default `immutable`. The agent creates a
private writable copy for that realization rather than hard-linking it. Pristine
base bytes remain verified and part of Release identity, while runtime mutations
are excluded from identity and deduplication, are not promised to survive an
upgrade or rollback, and are reported as drift. Preflight reports the extra
physical space required before staging. Poll-time drift reporting is bounded to
a capped file list plus size/mtime metadata; it does not recursively rehash a
large mutable resource on every poll. Durable writes still belong in
`persistentPaths`. `resources sync` performs best-effort linting for
`SaveResourceFile` and common file-write APIs, but dynamic code means the lint
cannot prove writability requirements.

## 3. Process supervision

Every target-local runtime binding declares exactly one supervisor:

```yaml
runtime:
  supervisor: txadmin # txadmin | systemd | manual
  rcon:
    host: 127.0.0.1
    port: 30120
    passwordSecret: {namespace: fivem, key: rcon-password}
  resourceState:
    host: 127.0.0.1 # numeric loopback only
    port: 30120
    tokenSecret: {namespace: fivem, key: health-token}
```

- `txadmin`: txAdmin owns the FXServer child process. The compatibility spike
  must prove supported refresh, resource restart, and full-restart mechanisms.
- `systemd`: the declared unit owns FXServer and uses the typed systemd adapter.
- `manual`: FXVCS can stage and validate but cannot claim an automatic full
  restart without an explicit supported adapter.

`resourceState` is required only when the portable Target requests the optional
`fxvcs-resource-state` readiness check. FXVCS generates
`resources/[fxvcs]/fxvcs-health`, ensures it after every enabled Release
resource, and requests `http://<loopback>:<port>/fxvcs-health/state` with a
Bearer token. The generated token convar uses `set`, never `setr`; its plaintext
is present only in an owner-readable generated cfg and memory, while its HMAC
version enters realization identity. Numeric loopback binding and an exact
16–512 byte printable token are mandatory.

V1 does not assume that systemd restart is correct for a txAdmin-supervised
server or that an RCON `quit` will be restarted. The txAdmin spike publishes a
versioned directive/process ownership matrix covering at least bootstrap cfg,
license key, endpoints, max clients, OneSync, txAdmin convars/resources, recipe
behavior, cfg editing, restart behavior, and distinct ports/profile ownership for
multiple txAdmin instances on one host.

Before activation the agent records the observed process generation and
supervisor state. A concurrent scheduled/operator/monitor restart aborts or
retries staging under bounded policy; it is never treated as a successful FXVCS
activation. Activation windows can exclude known scheduled restarts.

## 4. Target-provided runtime evidence

V1 manages resources and configuration against an existing target-provided
FXServer/txAdmin runtime. Releases do not pin, download, switch, retain, verify,
or roll back FXServer artifacts. First-class per-platform artifact lifecycle is
post-v1.

During staging and activation, the agent records best-effort observed runtime
evidence in the Deployment record rather than `sourceSetDigest`:

```yaml
observedRuntime:
  fxserverBuild: 12345
  fxserverVersionSource: info-json # version-file | info-json | process | unknown
  txadminVersion: 8.0.0
  systemResourcesDigest: sha256:...
```

The agent compares mutually corroborating sources when available: an installed
version/build file, the `/info.json` server/version string, and observed process
metadata. Disagreement is reported rather than selecting one silently. Runtime
resources from `RuntimeResourceCatalog` are validated against the actual
installed system-resources inventory during staging. Hooks may enforce local
runtime requirements, but core v1 does not claim that the runtime itself is
reproducibly supplied by the Release.

A later format generation may add optional immutable per-platform artifact pins
and target-owned `manage | verify` acquisition policy. That future capability
requires explicit Cfx distribution/legal review, hosting-provider evidence,
retention and rollback rules, txAdmin migration handling, and a repository
minimum-version upgrade.

## 5. Realization identity and local secret versions

Realization identity includes Release ID, `sourceSetDigest`, environment
revision, target content digest, hook/template digests, resolved external-
verifier binding/implementation digests, and provider version identifiers without
secret plaintext. Observed target-runtime versions are
deployment evidence in v1 and do not alter Release or realization identity.


Portable external-resource verifier contracts are resolved through the target's
agent-local allowlist. The realization snapshots a canonical digest of the
selected implementation ID/version and non-secret configuration; Deployment
evidence records it. Local paths, endpoints, credentials, and secret inputs do
not enter Release or `sourceSetDigest` identity. A missing or incompatible
binding fails before activation.

```yaml
verifiers:
  resource-present:
    implementation: rcon-resource-state
    implementationVersion: 1
    endpointRef: local-rcon
```

`endpointRef` and its credentials are agent-local binding data. The realization
digest identifies the implementation and safe configuration without persisting
secret material.

Cloud supplies immutable secret version IDs. Local `env` and `envfile` providers
derive opaque version fingerprints using an agent-local per-target random secret key and a
domain-separated HMAC over canonical target/namespace/key/value inputs. Raw
values and unkeyed hashes are never persisted. The local key lives only in
protected, backup-worthy agent state.

If that key is missing, the agent keeps the active realization running, reports
`secret-fingerprint-key-missing`, and refuses automatic reconciliation for a
target that uses local secrets. It never assumes unknown fingerprints are
unchanged. Restoring the protected state clears the fault. Otherwise,
`fxvcs agent rekey <target>` creates a new key and acknowledges re-realization
for that target only; it is not a host-wide shortcut. Rekey follows the target's
`activationWindow` and player-count gates because every affected target must
render a new realization and may restart.
