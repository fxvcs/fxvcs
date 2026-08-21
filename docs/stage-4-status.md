# Stage 4 status: target-side reconciliation engine

Status date: 2026-08-21

Stage 4 is **in progress**. The safety-critical contracts, target-local
primitives, public `ReconcileTarget` dry run, and locked systemd Apply path are
implemented and tested. Portable activation-window evaluation and
source-verified credentialed RCON status/resource lifecycle operations are now
implemented, along with the optional authenticated loopback resource-state
helper. The live Linux filesystem/txAdmin matrix remains fail-closed. No target
should be described as production-ready from this worktree.

The implementation contract and dependency order are recorded in
[`stage-4-plan.md`](stage-4-plan.md).

## Implemented

| Slice | Current evidence |
| --- | --- |
| Host contract | Strict `AgentConfig` schema, fixtures, semantic validation, host-local version negotiation, duplicate/nested-root rejection, and safety floors. |
| Target-local state | Target-bound `agent.db` with deployment attempts, full health phases, current/LKG observation, health samples, retry/rollback evidence, retained realizations, prune plans, and operation journals. The filesystem `current` link remains authoritative. |
| Identity and planning | Full SHA-256 lowercase unpadded-base32 realization IDs and a deterministic, side-effect-free reconciliation planner with stable refusal codes. |
| Materialized store | Bounded streaming publication, digest/size verification, no-replace durability, corruption refusal, immutable hard-link placement, and contained persistent bindings. |
| Safe rendering and secrets | In-process bounded `template.v1`, containment-checked output, opaque sensitivity-preserving inputs, a fixed `cfgQuote`-only function allowlist, explicit Target/Release path exposure, target-scoped HMAC versions, automatic plus legacy provider naming, envfile root/ownership/mode/symlink validation, and value-free envfile skeletons. |
| Activation and recovery | Per-target lock, strict relative `current -> releases/<id>` observation, journaled compare-and-swap activation, and conservative crash recovery. |
| Supervisor boundary | Typed capability contract, no-control manual adapter, argv-only systemd adapter, process-generation evidence, bounded/no-redirect FiveM `/dynamic.json` player observation from the agent-bound endpoint, source-verified bounded UDP RCON operations, and a txAdmin constructor that remains disabled without the required lab evidence. |
| Activation and lifecycle policy | Exact `window.v1` expressions with explicit IANA timezones and deterministic DST semantics; full-restart, hot-resource, and no-process-action classification from immutable prior realization evidence; exact typed RCON refresh/stop/restart/ensure operations; and process-generation invariants for every mode. |
| Readiness and rollback state | Bounded typed readiness checks, portable TCP and FiveM HTTP, credentialed RCON status, and an FXVCS-shipped authenticated numeric-loopback `fxvcs-health` resource ordered last. Its token is a non-replicated server convar, enters realization identity only through an HMAC version, and never enters metadata or durable health detail. Grace/threshold/soak tracking, restart invalidation, durable phase coordination, LKG promotion only after independent current observation, keep/none late recovery, rollback caps, and fresh-realization rollback use independent health proof. |
| External verification | Enabled external Release resources contribute their selected host verifier binding to realization identity and must pass an exact allowlisted implementation/version through the typed observe-only verifier registry before activation; safe canonical evidence digests are persisted without endpoint or credential data. |
| Realization and materialized pruning | Deterministic oldest-first dry runs, current/LKG/in-progress/operator/floor protections, exact plan IDs, apply-time revalidation, deletion-rate floors, rename-to-trash journaling, and recovery across rename and removal crash boundaries. Reference-safe materialized candidates include copy-mode metadata and staging references, are exposed separately in dry runs, and are deleted only after exact digest verification and locked re-observation. |
| Public inspection surface | Typed `DeploymentsList` and `DeploymentsPrune` SDK operations; `fxvcs deployments list [target]`; `fxvcs deployments prune <target> [--keep N] [--apply] [--plan ID]`; stable JSON and external SDK-consumer parity. |
| Reconcile resolution and Apply | Typed public `ReconcileTarget` dry run plus explicit Apply; AgentConfig binding plus coordination Target/Environment/Release resolution; pinned component and asset-root verification; symlink/gitlink and case-folded destination collision refusal; verified local materialized-content and conservative publication-remote availability; deploy-filesystem capacity and current-pointer observation; pinned/allowlisted hook-template verification and rendering; opaque secret versions. Apply repeats final preflight under the target lock, recovers activation journals, provisions only an initial secret HMAC key, enforces retry holds, publishes, installs non-destructive serverData links, journals CAS activation, proves a new process generation, persists readiness evidence, and never treats pointer equality alone as health. |
| Target content and realization staging | Target-side whole/chunked retrieval from required publication remotes with strict manifest, per-chunk, and whole-file verification; OID deduplication; bounded pinned-Git blob streaming; immutable hardlink views plus explicit executable/mutable copies; explicit file/directory persistent bindings into target-scoped shared storage with no source-seed guessing; canonical rebuild metadata; same-filesystem unselectable staging; OS-native atomic no-replace publication; and full tree/link verification before idempotent reuse. |
| FiveM runtime generation | Deterministic immutable `runtime/fxvcs.cfg` generation from the exact Release ensure order; disabled-resource omission; invalid, duplicate, and txAdmin-owned resource refusal; strict generated-fragment ownership validation; pinned hook fragments under `runtime/config`; generated-file inclusion in canonical assembly verification; and no persisted plaintext or unkeyed digest for secret-bearing output. |

## Remaining before Stage 4 production-readiness completion

- Run the Linux filesystem and txAdmin live-lab matrices. txAdmin control stays
  unavailable until E1-E11, E13, and E14 have recorded acceptable evidence.
- Reconcile the root README update with the intentional documentation migration
  on current main, then rerun all gates after merging main into this branch.

## Verification recorded

The following commands passed in the isolated Stage 4 worktree on 2026-08-20:

```text
go test ./...
go test -race ./internal/activation ./internal/deployment ./internal/health \
  ./internal/integrity ./internal/journal ./internal/materialize \
  ./internal/prune ./internal/readiness ./internal/reconcile \
  ./internal/rollback ./internal/schema ./internal/secrets ./internal/state \
  ./internal/supervisor ./internal/templatehook ./sdk ./internal/cli
go test -race ./internal/app ./internal/diskspace ./sdk
go test ./examples/sdk-consumer -run \
  'TestCLIAndSDKConsumerAgree|TestConsumerImportsOnlySDK' -v
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
git diff --check
```

After FiveM runtime generation and typed cfg rendering were added on
2026-08-21, the full suite and affected race packages were rerun successfully:

```text
go test ./...
go test -race ./internal/app ./internal/profile/fivem ./internal/secrets \
  ./internal/templatehook
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
git diff --check
```

After pinned hook/secret preflight and immutable rendering were added on
2026-08-21, `go test ./...` passed again. Apply remained disabled at that
historical checkpoint and was enabled only after the orchestration gates below.

After locked Apply, external verification, stable serverData links, and
materialized GC were added on 2026-08-21, these focused gates passed:

```text
go test ./internal/app -run 'TestApplyTarget' -v
go test ./internal/activation ./internal/prune ./internal/verifier \
  ./internal/reconcile ./internal/readiness ./internal/supervisor ./sdk
```

After Apply recovery/progress, external observe-only assembly, resumable exact
pruning, and the SDK golden update, the repository-wide gates passed on
2026-08-21:

```text
go test ./...
go test -race ./internal/activation ./internal/app ./internal/deployment \
  ./internal/health ./internal/materialize ./internal/prune \
  ./internal/readiness ./internal/reconcile ./internal/rollback \
  ./internal/runtimecheck ./internal/secrets ./internal/state \
  ./internal/supervisor ./internal/templatehook ./internal/verifier ./sdk \
  ./internal/cli
go vet ./...
golangci-lint run ./...  # pinned 2.12.2: 0 issues
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
govulncheck ./...        # 0 reachable vulnerabilities
git diff --check
```

The focused Apply acceptance fixtures were then expanded on 2026-08-21:

```text
go test ./internal/app -run \
  'TestApplyTarget(Invalidates|PreActivation|Halts|Builds|Recovers)' -v
```

They prove restart-race invalidation, pre-activation current preservation and
retry hold, and failed-rollback halt/no-oscillation behavior in addition to the
healthy and successful rollback paths. Repository-wide gates still need one
final rerun after current main integration; the isolated branch rerun is
recorded below.

At the final isolated-worktree checkpoint on 2026-08-21, after adding bounded
FiveM player-count observation and separating it from process-generation
observation, every local repository gate passed:

```text
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...  # pinned 2.12.2: 0 issues
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
govulncheck ./...        # 0 reachable vulnerabilities
git diff --check
```

This checkpoint still does not replace the required Linux filesystem/txAdmin
live-lab evidence. The repository gates must also be rerun after current main
is merged into this branch.

After portable activation windows, source-verified RCON, typed lifecycle
classification, interrupted-recovery gates, and keep late recovery were added,
the affected package and public-contract suites passed:

```text
go test ./internal/activationwindow ./internal/rcon ./internal/readiness
go test ./internal/reconcile ./internal/app
go test ./sdk -update
go test ./sdk ./examples/sdk-consumer
```

FiveM RCON framing was verified against citizenfx/fivem source commit
`f26e3aacb0f6fe8a8c76227a79688b0853dadb45`; live coexistence with txAdmin is
still experiment E4, not `lab` evidence.

At the final pre-integration checkpoint on 2026-08-21, after late-recovery and
final failure-result fixes, all local repository gates passed again:

```text
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...  # pinned 2.12.2: 0 issues
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
govulncheck ./...        # 0 called vulnerabilities
git diff --check
```

After closing the optional `fxvcs-resource-state` implementation gap, the same
complete local gate set passed again on 2026-08-21. Focused acceptance also
proved exact loopback/Bearer binding, strict bounded response decoding, last-in-
order helper generation, HMAC-only token identity, token-rotation
re-realization, metadata/evidence redaction, and runtime envfile skeletons:

```text
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...  # pinned 2.12.2: 0 issues
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
govulncheck ./...        # 0 called vulnerabilities
go test ./sdk -update
go test ./sdk ./examples/sdk-consumer
git diff --check
```

These results prove the checked-in unit, crash-fixture, race, vet, lint,
cross-build, vulnerability, API-golden, CLI, and external-consumer contracts.
They do not substitute for the remaining Linux filesystem/txAdmin live lab.
