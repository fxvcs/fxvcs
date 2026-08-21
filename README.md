# fxvcs

**Version control for FiveM resources — including the large assets Git was not
built to carry.**

fxvcs lets resource creators and server teams keep scripts, configuration, and
asset history together without filling a Git repository with maps, textures,
models, audio, and other large binaries. Source and small metadata stay in Git;
immutable asset content lives in a filesystem, S3, or S3-compatible store.

That means contributors can use familiar branches, commits, reviews, and tags
while fxvcs handles downloading assets, reclaiming local disk space, checking
integrity, and making sure required content is published before a Git push.

> [!WARNING]
> fxvcs is pre-release software. It is not yet ready to manage a production
> server. Stage 4 target reconciliation is available for evaluation, but its
> Linux/txAdmin live-lab matrix remains unverified and committed formats or
> commands may still change.

## Why fxvcs?

- **Keep the whole resource versioned.** Track code and large binary assets as
  one logical change instead of passing asset folders around separately.
- **Keep Git fast.** Small deterministic placeholders are committed in place of
  binary content, so clones and normal Git operations stay lightweight.
- **Download only when needed.** `fxvcs load` restores real files and
  `fxvcs unload` safely turns unchanged files back into placeholders.
- **Upload only what changed.** Content-defined chunking reuses unchanged parts
  of large files and avoids uploading the same content twice.
- **Share storage on your terms.** Use a shared directory, Amazon S3, or an
  S3-compatible service. Credentials stay on each contributor's computer.
- **Compose real projects.** Stack repositories can pin multiple resource
  repositories into an immutable release with portable release metadata.
- **Automate without scraping terminal output.** The `fxvcs` CLI offers stable
  JSON and the public Go SDK exposes typed requests, results, errors, and
  progress.

## How it works

```text
Your Git repository                 Shared asset storage
-------------------                 --------------------
scripts and configuration   ──────▶ normal Git remote
small fxvcs placeholders     ──────▶ normal Git remote
large immutable content      ──────▶ filesystem / S3-compatible storage
```

When Git stages a tracked asset, fxvcs records a compact pointer instead of the
asset bytes. Your worktree can still contain the real file. On another machine,
`fxvcs load` verifies the pointer, downloads the content, and restores the file.
Dirty files are protected: unload, repair, and other destructive-looking
operations refuse to overwrite local changes.

## Quick start

### Requirements

- Git 2.36 or newer
- [mise](https://mise.jdx.dev) for the pinned Go toolchain

Tagged releases attach archives for Linux, macOS, and Windows on AMD64 and
ARM64, plus a `SHA256SUMS` file. Until the first release is published, build
the current development version from source:

```bash
git clone <this-repository-url>
cd fxvcs
mise install
mise run install
fxvcs version
```

Verify a downloaded release archive before extracting it:

```bash
sha256sum --check SHA256SUMS
```

On macOS, use `shasum -a 256 <archive>` and compare the result with the matching
line in `SHA256SUMS`.

### Start a resource repository

Run these commands inside a Git repository containing your FiveM resources:

```bash
# Write repository configuration and Git integration; nothing is committed.
fxvcs init --profile fivem

# Choose shared asset storage.
fxvcs storage add --name primary --path /srv/fxvcs-storage

# Or use an S3-compatible bucket instead:
# fxvcs storage add --name primary \
#   --bucket fxvcs-assets \
#   --endpoint https://s3.example.com
# fxvcs storage login primary --access-key-id YOUR_KEY

# Start tracking large assets, then commit normally.
fxvcs track --glob "stream/**"
git add .
git commit -m "set up fxvcs asset tracking"

# Publish asset content before pushing its pointers.
fxvcs objects publish
git push
```

On another computer:

```bash
fxvcs clone <repository-url>
cd <repository-directory>
fxvcs load
fxvcs status
```

Run `fxvcs --help` to see the command groups and
`fxvcs <command> --help` for examples. Add `--json` for the stable
machine-readable interface or `--verbose` when diagnosing a problem.

### Git launcher installation

Git filters and hooks must keep working when the terminal or fxvcs Desktop is
closed, so setup installs a stable per-user fxvcs CLI/Engine launcher and
records its absolute path in each repository:

| Platform | Launcher path |
| --- | --- |
| Linux | `${XDG_DATA_HOME:-$HOME/.local/share}/fxvcs/bin/fxvcs` |
| macOS | `$HOME/Library/Application Support/fxvcs/bin/fxvcs` |
| Windows | `%LOCALAPPDATA%\fxvcs\bin\fxvcs.exe` |

The fxvcs CLI and fxvcs Desktop may both maintain this launcher. Updates are
atomic and newest-version-wins: an older caller never downgrades a newer
installed helper. Existing macOS installations using the former
`~/.local/share/fxvcs/bin/fxvcs` default are kept working while repositories
are migrated to the native Application Support location. Run
`fxvcs launcher ensure` to install or verify the shared helper explicitly.

## From resources to releases

fxvcs separates a **component repository** from a **stack repository**:

- A component repository owns one or more related resources and their assets.
- A stack repository combines independently versioned components, validates
  how they fit together, and creates an immutable release.

```bash
mkdir my-server-stack && cd my-server-stack
git init
fxvcs stack init
fxvcs stack add <component-repository-url> --mount resources/my-resource
fxvcs stack sync
fxvcs stack validate
fxvcs release create v0.1.0
```

Use `fxvcs release inspect`, `fxvcs release export`, and
`fxvcs release import` to review and move release metadata. Locked systemd
target reconciliation and Apply are available for evaluation; production
activation remains gated on Linux/txAdmin live evidence.

## Project status

The current pre-release includes:

- resource-repository setup, clone, upgrade, tracking, migration, and repair;
- Git filter, diff, merge, and pre-push integration;
- filesystem and S3-compatible asset storage;
- differential chunked objects and a bounded, self-verifying local cache;
- FiveM resource discovery and manifest inspection;
- multi-repository stacks, immutable releases, and desired target/environment
  documents;
- fail-closed target reconciliation with locked systemd Apply, bounded health
  checks, rollback, typed resource lifecycle operations, and deployment pruning;
- a public v0 Go SDK and stable `fxvcs --json` output.

See the [Stage 4 status](docs/stage-4-status.md) for implemented evidence and
remaining acceptance gates, and the [Stage 4 plan](docs/stage-4-plan.md) for its
implementation contract. Work still in progress includes a long-running pull
agent and continuous monitoring, live txAdmin compatibility validation, signing
and distribution hardening, and object garbage collection. Hosted services are
optional future work; the standalone product is intended to remain useful on
its own.

## Contributing

fxvcs welcomes bug reports, documentation improvements, test cases, design
feedback, and focused code contributions. Good first contributions include:

- reproducing a problem on Linux, macOS, or Windows;
- improving command examples and error messages;
- adding malformed, compatibility, or crash-recovery fixtures;
- extending S3-compatible provider coverage;
- improving FiveM resource discovery and validation tests.

Before starting a large feature or format change, open an issue so the approach
can be discussed before significant work is invested. Then read
[CONTRIBUTING.md](CONTRIBUTING.md) for setup, repository rules, test commands,
and the pull-request checklist.

## For SDK consumers

The supported integration boundary is the `fxvcs.dev/fxvcs/sdk` Go package.
Packages below `internal/` are implementation details and cannot be imported by
external modules. The v0 SDK is additive where possible, but fxvcs remains
pre-release and does not yet promise v1 compatibility.

See [`examples/sdk-consumer`](examples/sdk-consumer) for a minimal integration.
The fxvcs Desktop packaging and launcher contract is documented in
[`DESKTOP_LAUNCHER_INTEGRATION.md`](DESKTOP_LAUNCHER_INTEGRATION.md).

## License

An external-distribution license has not yet been approved, so this repository
does not currently include a `LICENSE` file. Do not redistribute the project
until that changes.
