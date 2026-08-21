# Contributing to fxvcs

Thanks for helping improve fxvcs. Contributions can be code, tests,
documentation, bug reports, compatibility findings, or feedback from real
FiveM resource workflows.

fxvcs protects users' source files and asset data, so correctness and failure
behavior matter more than making a change look small. Please read this guide
before opening a pull request.

## Before you start

- Search existing issues before filing a new one.
- Use an issue to describe bugs, expected behavior, and a minimal reproduction.
- Discuss large features, new committed formats, new dependencies, and
  architectural changes before implementing them.
- Keep pull requests focused. Do not mix unrelated cleanup with a behavior
  change.
- Never include credentials, proprietary assets, or absolute paths from your
  computer in a report or fixture.

## Development setup

fxvcs uses [mise](https://mise.jdx.dev) to pin Go and development tools.

Requirements:

- Git 2.36 or newer
- mise
- a POSIX shell; use Git Bash for mise tasks on Windows

```bash
git clone <your-fork-url>
cd fxvcs
mise install
mise run build
./bin/fxvcs version
```

Useful tasks:

```bash
mise tasks             # list every task and its description
mise run test-short    # fast local test pass
mise run test          # full Go test suite
mise run test-race     # race detector; requires cgo, not run on Windows CI
mise run fmt           # format Go files
mise run lint          # golangci-lint
mise run ci            # format check, vet, lint, and tests
mise run tidy-check    # prove go.mod and go.sum are already tidy
```

CI runs on Linux, macOS, and Windows. Run `mise run ci` before submitting when
possible, and state which checks you could not run.

## Repository principles

### Protect user data

- Validate before writing and fail closed on ambiguous input.
- Never overwrite a dirty worktree file.
- Do not leave partial output that could be mistaken for valid state.
- Treat repository-provided paths as untrusted until they pass the containment
  helpers in `internal/pathsafe`.
- Assume interruption can happen between any two filesystem or database
  operations. Tests for destructive or recovery paths should cover that.

### Keep boundaries clear

- The `fxvcs` CLI and integrations call the public `sdk` package.
- Engines and storage implementations live under `internal/`.
- The SDK returns typed data and errors; it does not print, prompt, or exit.
- Do not add delivery-stage vocabulary such as "Stage 4" to Go names, logs,
  errors, or comments. Describe the behavior instead.

### Keep formats compatible

Committed documents, pointers, object encodings, Git integration, and local
SQLite state are independently versioned. Changes must:

1. reject unsupported or newer inputs before writing;
2. preserve existing supported inputs;
3. include valid, malformed, and upgrade fixtures where applicable;
4. be deterministic and idempotent;
5. update compatibility reporting and public SDK types when needed.

Do not edit `go.mod` or `go.sum` casually. A new dependency expands the
security and maintenance surface; explain in the pull request why existing
code or dependencies cannot solve the problem.

## Tests and fixtures

Put small, reviewable fixtures in the location owned by the behavior:

| Location | Purpose |
| --- | --- |
| `spec/schemas/v1alpha1/` | Schemas for committed documents |
| `spec/fixtures/documents/` | Valid minimal and full documents |
| `spec/fixtures/malformed/` | Inputs that must be rejected |
| `spec/fixtures/golden/` | Exact canonical bytes and digests |
| `spec/fixtures/upgrade/` | Older inputs and expected current outputs |
| `spec/fixtures/crash/` | Interrupted-state recovery cases |
| `spec/fixtures/git-integration/` | Pointer-aware Git behavior |
| `spec/fixtures/fivem/` | FiveM discovery and manifest cases |
| `<package>/testdata/` | Package-private fixtures |

Fixtures are LF-only unless line endings are the subject of the test. Keep
binary fixtures small; generate large test data at runtime.

Tests must be portable:

- use `t.TempDir()` and `filepath.Join`;
- execute programs directly instead of relying on shell scripts;
- do not assume a case-sensitive filesystem;
- isolate Git configuration and disable interactive prompts;
- skip only platform capabilities that truly do not exist, while still testing
  the shared logic.

## Golden files and public API changes

Golden files only change intentionally. Regenerate the currently supported
goldens with:

```bash
mise run fixtures-update
```

Review the resulting diff and explain it in the pull request. Never use
`-update` as a way to make a failing test pass without understanding the
change.

The public SDK surface is pinned by `sdk/testdata/api-v0.golden`.
`TestAPISurfaceIsPinned` will fail when an export changes. Public API changes
must be deliberate, additive where possible, tested from a consumer's point of
view, and accompanied by a reviewed golden update.

## Pull request checklist

- [ ] The issue and user-visible behavior are clearly described.
- [ ] The change is focused and contains no unrelated formatting or cleanup.
- [ ] New behavior has tests and appropriate fixtures.
- [ ] Failure paths protect dirty files and reject unsafe paths.
- [ ] New dependencies are justified.
- [ ] Public SDK or committed-format changes include compatibility coverage.
- [ ] Golden-file changes are reviewed and explained.
- [ ] `mise run ci` passes, or missing checks are called out.
- [ ] `mise run tidy-check` leaves `go.mod` and `go.sum` unchanged.
- [ ] No credentials, private assets, or host-specific paths are present.

## Reporting security problems

Do not publish credentials, private asset content, or a working exploit in a
public issue. If no private security-reporting channel is available for the
repository yet, open a minimal issue asking a maintainer for a private contact
without including sensitive details.

## Publishing a release

Releases are maintainer-only. Release tags must be v-prefixed semantic versions
such as `v0.1.0` or `v0.2.0-rc.1`. An annotated, signed tag is preferred:

```bash
git tag -s v0.1.0 -m "fxvcs v0.1.0"
git push origin v0.1.0
```

Pushing the tag runs the release workflow and creates the GitHub Release.
Publishing a GitHub Release from an existing tag also runs the workflow. It
repeats the CI checks, builds six Linux, macOS, and Windows archives, verifies
the embedded version, and attaches the archives with `SHA256SUMS`. Do not build
or upload release archives by hand.

## Licensing

The project does not yet include an approved external-distribution license.
Only submit work you have the right to contribute, and do not assume the
repository may be redistributed until a `LICENSE` file is added.
