# Git integration fixtures

Golden inputs/outputs for the pointer-aware diff and merge drivers
(`internal/filter/drivers_test.go`; regenerate with `go test ./internal/filter -update`).

- `pointer-base.txt`, `pointer-ours.txt`, `pointer-theirs.txt`: v1 pointers for
  three versions of one asset (contents "asset version 1", "asset version 2
  (ours)", "asset version 3 (theirs)" under `domain_test_01`).
- `merge-take-ours.expected`, `merge-take-theirs.expected`: clean merge results
  when only one side changed.
- `merge-conflict.expected`, `merge-conflict-no-base.expected`: the conflict
  layout written to `%A` when both sides changed (with and without a pointer
  base).
- `diff-*.expected`: diff-driver output for pointer vs pointer, added file, and
  pointer vs hydrated working file.
