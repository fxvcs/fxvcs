package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers driver "sqlite"
)

// DriverName is the database/sql driver used for every FXVCS SQLite file.
const DriverName = "sqlite"

// FileOptions tune MigrateFile.
type FileOptions struct {
	Run RunOptions
	// KeepBackup retains <db>.bak-<from> after a successful swap. Required for
	// Authoritative databases; optional for Derived ones.
	KeepBackup bool
	// Prepare runs on the freshly opened staging copy before the plan (for
	// example to set busy_timeout). May be nil.
	Prepare func(db *sql.DB) error
}

// FileReport describes what MigrateFile did.
type FileReport struct {
	From, To   int
	Applied    bool
	BackupPath string // non-empty when a backup was kept
}

// MigrateFile upgrades the SQLite file at path from its current user_version
// to reg.Latest() using a transactional swap:
//
//  1. read user_version and Plan (nothing written; ErrNewerThanSupported here
//     leaves the file byte-identical);
//  2. copy the database to <db>.migrating with VACUUM INTO (a consistent
//     snapshot even in WAL mode);
//  3. Run the plan on the copy, then close it so its WAL is checkpointed;
//  4. fsync the copy and its directory;
//  5. rename the original to <db>.bak-<from> and the copy over the original;
//     delete the backup unless KeepBackup.
//
// The caller must hold the database's exclusive lock (see internal/journal);
// if another connection still has a write-ahead log open, ErrBusy is returned
// before the swap and nothing is replaced.
func MigrateFile(ctx context.Context, path string, reg *Registry, opts FileOptions) (FileReport, error) {
	var rep FileReport
	if _, err := os.Stat(path); err != nil {
		return rep, err
	}
	src, err := sql.Open(DriverName, path)
	if err != nil {
		return rep, err
	}
	src.SetMaxOpenConns(1)
	from, err := UserVersion(src)
	if err != nil {
		src.Close()
		return rep, err
	}
	rep.From, rep.To = from, reg.Latest()
	plan, err := reg.Plan(from, reg.Latest())
	if err != nil {
		src.Close()
		return rep, err
	}
	if plan.Empty() {
		src.Close()
		return rep, nil
	}
	staging := StagingPath(path)
	_ = os.Remove(staging)
	// VACUUM INTO refuses to overwrite; the target must not exist.
	if _, err := src.ExecContext(ctx, "VACUUM INTO ?", staging); err != nil {
		src.Close()
		return rep, fmt.Errorf("migrate: snapshot %s: %w", filepath.Base(path), err)
	}
	// Checkpoint and release the source so no WAL survives the swap.
	_, _ = src.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err := src.Close(); err != nil {
		_ = os.Remove(staging)
		return rep, err
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		_ = os.Remove(staging)
		return rep, fmt.Errorf("%w: %s-wal is not empty", ErrBusy, filepath.Base(path))
	}

	dst, err := sql.Open(DriverName, staging)
	if err != nil {
		_ = os.Remove(staging)
		return rep, err
	}
	dst.SetMaxOpenConns(1)
	if opts.Prepare != nil {
		if err := opts.Prepare(dst); err != nil {
			dst.Close()
			_ = os.Remove(staging)
			return rep, err
		}
	}
	if err := Run(ctx, dst, plan, opts.Run); err != nil {
		dst.Close()
		_ = os.Remove(staging)
		return rep, err
	}
	if _, err := Verify(ctx, dst); err != nil {
		dst.Close()
		_ = os.Remove(staging)
		return rep, err
	}
	_, _ = dst.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err := dst.Close(); err != nil {
		_ = os.Remove(staging)
		return rep, err
	}
	if err := fsyncFile(staging); err != nil {
		_ = os.Remove(staging)
		return rep, err
	}
	backup := BackupPath(path, from)
	if err := os.Rename(path, backup); err != nil {
		_ = os.Remove(staging)
		return rep, fmt.Errorf("migrate: move original aside: %w", err)
	}
	if err := os.Rename(staging, path); err != nil {
		// Put the original back; the staging copy is left for inspection.
		if rerr := os.Rename(backup, path); rerr != nil {
			return rep, errors.Join(fmt.Errorf("migrate: swap failed: %w", err), fmt.Errorf("migrate: restore original failed (backup at %s): %w", backup, rerr))
		}
		return rep, fmt.Errorf("migrate: swap failed, original restored: %w", err)
	}
	_ = fsyncDir(filepath.Dir(path))
	rep.Applied = true
	if opts.KeepBackup {
		rep.BackupPath = backup
	} else {
		_ = os.Remove(backup)
	}
	return rep, nil
}

func fsyncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Directory fsync is unsupported on some platforms (Windows); ignore.
	_ = d.Sync()
	return nil
}
