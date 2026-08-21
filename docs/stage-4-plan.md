# Stage 4 plan: target-side reconciliation engine

Status: approved implementation plan

Stage 4 turns an already-authorized Target, Environment, and immutable Release
into one safely staged, activated, observed, and retained target realization. It
is a reusable one-target engine. Stage 5 adds Git/Cloud polling, the long-running
agent process, assignment, retry/rekey commands, and continuous monitoring.

This plan is an implementation gate. Stage 4 work proceeds in the dependency
order below and does not enable a target mutation until the applicable contract,
preflight, durability, and recovery tests pass.

## 1. Scope and completion boundary

Stage 4 delivers:

- strict loading and validation of versioned, agent-local host bindings;
- a pure reconciliation plan for an already-selected desired target tuple;
- out-of-place verified materialization and deterministic configuration
  rendering;
- atomic realization activation through a supervisor capability boundary;
- deployment-time readiness, held failure, and bounded rollback behavior;
- crash-recoverable per-target observed state and deployment evidence;
- reference-safe, plan-first local realization pruning;
- typed SDK operations used by the future agent and the
  `fxvcs deployments list|prune` CLI surface.

Stage 4 does not deliver:

- Git or Cloud polling, agent enrollment, target assignment, or a daemon;
- `fxvcs agent run|reconcile|doctor|retry|rekey` or continuous monitoring;
- a direct `fxvcs deploy` command or inbound SSH deployment;
- signature enforcement, remote object garbage collection, or Cloud authority;
- FXServer artifact acquisition/switching or a Windows target agent;
- arbitrary repository-defined executables or txAdmin browser-session
  automation.

## 2. Locked architecture decisions

### 2.1 Agent-local configuration

`AgentConfig` is versioned but never committed to a component or coordination
repository. The Linux default is `/etc/fxvcs/agent.yaml`; callers may override
it with `--agent-config` or `FXVCS_AGENT_CONFIG`. Strict schema and semantic
validation happen before opening target state or touching a deployment root.

Validation rejects duplicate target bindings, duplicate or nested deployment
roots, overlapping `serverData`/`deployRoot`/`txData`, supervisor-specific
missing fields, unknown provider/hook types, duplicate endpoint ownership, and
desired safety floors weaker than the host-local floor.

### 2.2 Authority and local state

Each target owns `<deployRoot>/state/agent.db`. The effective `current`
filesystem indirection is the authority for which realization is selected;
SQLite indexes deployment history, operation phase, retry holds, health
samples, prune plans, and evidence. Realization metadata is also durably stored
inside each realization so the database can be rebuilt.

The engine never uses a database refcount as sole proof that content may be
deleted. Reconciliation and pruning re-observe the current indirection,
in-progress journals, retained realization manifests, and last-known-good pins
under the target lock.

### 2.3 Filesystem layout and activation

The normative layout remains `docs/spec/runtime-layout.md`. Encoded transport
objects and reconstructed materialized files are separate stores. Missing
materialized files are written to a same-directory temporary file, bounded and
hashed while streaming, flushed, made read-only, atomically published without
replacement, and followed by a parent-directory durability barrier. An
existing digest-named file is byte-verified; existence is never accepted as
proof.

Realization trees are assembled out of place. Immutable files hard-link to the
deployment-root materialized store. Optional reflink support is capability
gated. Ordinary copying is an explicit preflighted fallback and is always used
for `mutableMode: copy` resources. Activation atomically replaces only the
`current` indirection and copies no asset bytes.

Declared `persistentPaths` become agent-created, containment-checked relative
symlinks into `<deployRoot>/shared/persistent/<repositoryID>/<resource-path>`.
Each declaration has an explicit `persistentPathKinds` entry (`file` or
`directory`). V1 creates empty runtime-owned storage and refuses committed seed
bytes at that path; it never guesses kind or seed semantics.
Source-controlled symlinks are never materialized. Collisions, overlaps,
escapes, and destinations outside the exact target-scoped persistent root fail
before activation. An FTP/SFTP view may expose declared persistent paths but
must not expose the rest of `shared`, agent state, runtime secrets, or `txData`.

### 2.4 Reconciliation and recovery

The public Stage 4 application operation accepts a coordination checkout,
target name, and agent-config path. It resolves immutable desired inputs, then
calls a pure planner before acquiring the mutation lock. The future Stage 5
agent invokes this same SDK operation after polling determines that work may be
needed.

The target lock covers journal recovery, final preflight, materialization,
rendering, activation, health state changes, rollback, and prune application.
Recovery is conservative:

- incomplete pre-activation work preserves the previous current realization;
- a fully assembled but unselected realization may be reused after validation;
- an ambiguous current pointer, process generation, or journal state is held
  for diagnosis rather than guessed;
- post-switch recovery re-observes the filesystem and supervisor before
  advancing state;
- rollback constructs a new realization of the prior Release under the current
  authorized environment, hooks, verifier bindings, and secret policy.

### 2.5 Templates, hooks, and secrets

V1 supports only the built-in `template.v1` hook. It runs in-process with no
network access, no raw file/URL inputs, a fixed function allowlist, bounded
input/output, deterministic ordering, and destination containment. Repository
content cannot introduce a command or executable path.

Typed inputs accept exactly one of `secret`, `target`, `release`, or `literal`.
The initial local secret providers are `env` and `envfile`; provider selection
has no implicit fallback. Envfile paths must be absolute, outside every source,
coordination, deployment, and txData root, free of symlink traversal, and
protected by safe ownership and permissions.

Local secret versions are domain-separated HMAC-SHA-256 fingerprints using one
protected per-target random key. Plaintext and unkeyed hashes are never
persisted or logged. If the key is missing, the active realization remains in
place and automatic reconciliation stops with
`secret-fingerprint-key-missing`.

`fxvcs.cfg` generation is deterministic from Release activation order and typed
inputs. It rejects txAdmin-owned/forbidden directives, emits secret-bearing
fragments outside `resources/`, and never edits the stable operator-owned
`server.cfg` bootstrap.

### 2.6 Supervisor and txAdmin boundary

Supervisors expose typed capabilities: observe process generation, apply
resource actions, request or perform a full restart, and report whether an
action is unsupported. Implementations are `manual`, `systemd`, RCON resource
actions, and txAdmin observation. No implementation constructs an arbitrary
shell command from repository content.

txAdmin is the primary FiveM operating model, but every mechanism remains
disabled until its row in `docs/spec/txadmin-compatibility-matrix.md` has live
`lab` evidence for the supported version range. Stage 4 runs experiments
E1-E11, E13, and E14. It does not automate session-cookie/CSRF login. Verified
RCON is preferred for resource-level changes. A full-restart-required plan is
reported as unsupported unless a safe headless or explicit external-restart
handshake is proven; changing `current` alone is never reported as successful
activation.

### 2.7 Health, rollback, and pruning

The existing pure state machine in `internal/health` remains authoritative.
Deployment phase and `retryHeld` are persisted separately. Readiness checks are
typed, independently bounded, and report which interface they proved. A
realization becomes last-known-good only after thresholds and soak pass.

`keep`, `none`, and `rollback` follow `docs/spec/health-and-rollback.md`.
Desired state is never mutated. Rollback caps and host safety floors cannot be
relaxed by Target policy. A competing supervisor generation invalidates an
attempt and cannot be counted as activation success.

Pruning is dry-run by default and emits an exact candidate set with paths,
realization IDs, digests, and estimated reclaimed bytes. Apply revalidates the
plan under the target lock and protects current, in-progress, last-known-good,
minimum-retention, and explicitly retained realizations. Removal is journaled
and crash-restartable. Local pruning never deletes coordination Releases or
remote objects.

## 3. Public application surface

The public SDK gains additive v0 contracts for:

- `ReconcileTarget`: resolve and reconcile one already-authorized standalone
  target through the Stage 4 engine;
- `DeploymentsList`: list durable local deployment attempts and current/LKG
  state for bound targets;
- `DeploymentsPrune`: produce a dry-run plan or apply an exact validated plan;
- typed reconciliation plan, lifecycle action, deployment phase, health
  evidence, prune candidate, and refusal details;
- progress phases for preflight, materialization, rendering, activation,
  readiness, rollback, and pruning.

The CLI exposes only `fxvcs deployments list` and
`fxvcs deployments prune <target> [--apply] [--keep N]` in Stage 4. CLI text,
prompts, confirmation, and exit codes remain presentation concerns. The SDK
never prints, prompts, reads stdin, or calls `os.Exit`.

## 4. Dependency-ordered implementation slices

### 4.0 Contract and lab gate

- Add the `AgentConfig` schema, loader, fixtures, and semantic validation.
- Correct version-negotiation documentation so agent-local configuration is
  versioned without being described as repository-committed state.
- Define supervisor capability and lifecycle-plan types.
- Add the persistent-path contract and fixtures.
- Run and record the txAdmin live-lab matrix before enabling txAdmin behavior.

Exit: malformed or unsafe bindings fail without creating/opening target state;
every enabled txAdmin mechanism has binary lab evidence.

### 4.1 Per-target state and recovery

- Add the agent database schema/migration and durable target lock.
- Store deployments, phase/hold state, health samples, LKG, operation journals,
  and prune plans.
- Reconcile the database from current and realization metadata.
- Add deterministic failpoints before and after each database/filesystem
  durability boundary.

Exit: every crash fixture either preserves the previous active realization or
recovers an unambiguous completed transition; ambiguity fails closed.

### 4.2 Pure reconciliation planner

- Resolve Target, Environment, Release, component pins, agent binding, hooks,
  verifiers, secrets, retention, health policy, and current realization.
- Compute realization identity, missing content, disk requirements, lifecycle
  actions, activation gates, and prune pressure without writes.
- Return stable refusal codes for unavailable content, unsafe bindings,
  unsupported lifecycle actions, player/window gates, and insufficient space.

Exit: identical immutable inputs produce byte-identical plans and no preflight
test observes a filesystem or database mutation.

### 4.3 Verified materialization

- Walk pinned component trees without using a mutable worktree.
- Reject Git symlinks, gitlinks, traversal, reserved names, case collisions,
  overlapping mounts, and unexpected pointer domains.
- Reconstruct and verify missing digest-keyed ordinary files.
- Assemble immutable hard-link views, copy-mode resources, persistent bindings,
  runtime resources, and realization metadata out of place.

Exit: unchanged digests share one physical file, corrupt existing files are
never reused, and partial/interrupted output is never selectable.

### 4.4 Rendering and validation

- Resolve typed inputs and safe secret versions.
- Implement the deterministic `template.v1` renderer and `cfgQuote`.
- Generate and validate FiveM runtime configuration and envfile skeletons.
- Resolve allowlisted verifier implementations and record safe binding digests.
- Record observed runtime evidence and validate required runtime resources.

Exit: hook/secret/verifier/runtime failure leaves the prior realization active;
logs, state, errors, and generated non-secret evidence contain no secret bytes.

### 4.5 Activation and lifecycle

- Implement capability-gated manual, systemd, RCON, and txAdmin-observation
  adapters with bounded operations and redacted diagnostics.
- Classify no-op, hot-resource, and full-restart changes.
- Record process generation before switching current, apply the exact lifecycle
  plan, and re-observe generation before accepting activation.
- Integrate stable `serverData` links without modifying unrelated operator
  configuration.

Exit: activation copies no asset bytes, every pre-switch failure preserves the
old current target, and supervisor races invalidate rather than complete work.

### 4.6 Readiness and rollback

- Implement bounded TCP, FiveM HTTP, RCON status, and optional authenticated
  resource-state checks.
- Persist thresholds, grace, soak, phase, retry hold, and rollback-window data.
- Rebuild rollback realizations under current authorized local policy.
- Continue health evaluation only for active-unhealthy desired tuples as
  specified.

Exit: keep/none/rollback, late recovery, rollback caps, failed rollback, player
gates, activation windows, and restart races match the normative state machine.

### 4.7 Retention and pruning

- Compute reference-safe automatic/manual prune plans.
- Enforce target policy combined with stricter host-local floors.
- Add dry-run and exact-plan application with stale-plan rejection.
- Reconcile interrupted removals and bounded deletion-rate accounting.

Exit: active, staging, LKG, retained, and floor-protected realizations survive
all normal, concurrent, and crash-recovery cases.

### 4.8 SDK, CLI, and acceptance

- Add the typed SDK operations and API golden changes.
- Add `deployments list` and `deployments prune` CLI adapters and JSON output.
- Prove SDK/CLI parity with an external consumer fixture.
- Run representative old-to-new, pre-activation failure, keep, rollback,
  restart, recovery, and prune scenarios over multi-repository FiveM fixtures.
- Update README status and publish `docs/stage-4-status.md` with exact evidence
  and remaining gaps.

Exit: full, race, lint, security, cross-platform compile, and Linux live-lab
gates pass; no target is described as production-ready without the recorded
txAdmin and filesystem evidence.

## 5. Required verification matrix

At minimum, tests cover:

- same-filesystem hard links, optional reflinks, explicit copy fallback, low
  disk, and different-filesystem refusal/fallback;
- corrupt digest-name collisions, truncated/chunked objects, decompression
  bounds, cache/object-store outage, and cancelled transfers;
- source and destination symlinks, path replacement races, case collisions,
  nested mounts, persistent-path escapes, and malicious templates;
- crash before/after materialized-file publication, realization publication,
  database commit, current switch, lifecycle action, health transition,
  rollback switch, and prune removal;
- duplicate targets/deploy roots, simultaneous target operations, and separate
  targets sharing only the authorized immutable cache;
- env/envfile permission errors, missing aliases, missing HMAC key, secret
  rotation, cfg injection, and log/error redaction;
- manual/systemd/RCON capability gaps, txAdmin scheduled/operator restarts,
  process-generation changes, and unsupported full restart;
- healthy activation, startup timeout, keep, none, rollback, late recovery,
  rollback cap, failed rollback, player gate, activation window, and soak;
- dry-run pruning, stale plan, active/LKG/in-progress protection, safety floors,
  deletion-rate limits, interruption, and exact post-apply absence.

Each implementation slice begins with focused failing tests and records the
corresponding passing evidence before the next destructive capability is
enabled. Stage 4 is complete only when the current Release is never partially
materialized, pre-activation failure preserves the prior active Release,
rollback never oscillates, and pruning cannot delete protected content.
