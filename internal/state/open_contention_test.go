package state

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentOpenDoesNotReportAHealthyDatabaseAsCorrupt is the regression
// for advice that would have destroyed state to fix nothing.
//
// Opening runs read-only probes before it writes anything, so that a damaged
// file is diagnosed without being touched. Those probes ran on a connection
// that had no busy timeout yet — the timeout was set with the pragmas that
// come after them — so a concurrent opener rebuilding the WAL index, or a
// writer mid-commit, failed the first read instantly. Every probe failure was
// then reported as corruption, and the guidance for that is "delete state.db
// and rebuild" or, for the authoritative publication ledger, "restore from
// backup".
//
// Two ordinary fxvcs processes are enough to hit it: a command and the Git
// filter process, or two terminals.
func TestConcurrentOpenDoesNotReportAHealthyDatabaseAsCorrupt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := Open(ctx, path, Options{Create: true, InstalledVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(ctx, PathRow{Path: "a.bin", Intent: IntentHydrated, IndexPointerDigest: "d", WorkingDigest: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Simultaneous bursts: each round starts every opener at once, which is
	// what makes one of them rebuild the WAL index while the others read.
	const rounds, workers = 120, 8
	var corrupt, busy int
	var first error
	var mu sync.Mutex
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := Open(ctx, path, Options{InstalledVersion: "test"})
				if err != nil {
					mu.Lock()
					switch {
					case errors.Is(err, ErrCorrupt):
						corrupt++
						if first == nil {
							first = err
						}
					case errors.Is(err, ErrBusy):
						busy++
					}
					mu.Unlock()
					return
				}
				_ = d.Close()
			}()
		}
		wg.Wait()
	}
	if corrupt > 0 {
		t.Fatalf("a healthy database was reported corrupt %d/%d times: %v", corrupt, rounds*workers, first)
	}
	// Reporting busy is acceptable — it is true, and its advice is to retry —
	// but with a timeout in place it should be rare rather than routine.
	if busy > rounds*workers/10 {
		t.Fatalf("reported busy %d/%d times; the timeout is not being applied", busy, rounds*workers)
	}
	// And the file really is fine.
	d, err := Open(ctx, path, Options{InstalledVersion: "test"})
	if err != nil {
		t.Fatalf("healthy database will not open: %v", err)
	}
	var res string
	if err := d.SQL().QueryRow("PRAGMA integrity_check").Scan(&res); err != nil || res != "ok" {
		t.Fatalf("integrity_check %q %v", res, err)
	}
	_ = d.Close()
}
