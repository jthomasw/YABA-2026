package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// runBackup takes one verified snapshot, or lists the existing ones.
func (a *app) runBackup(dbPath, dir, uploadDir string, keep int, list, acceptEmpty bool) error {
	if list {
		found, err := db.Snapshots(dir)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			fmt.Fprintf(a.out, "No snapshots in %s\n", dir)
			return nil
		}
		fmt.Fprintf(a.out, "%d snapshot(s) in %s, oldest first:\n", len(found), dir)
		for _, p := range found {
			size := int64(0)
			if info, err := os.Stat(p); err == nil {
				size = info.Size()
			}
			receipts := ""
			if _, err := os.Stat(strings.TrimSuffix(p, ".db") + "-uploads.zip"); err == nil {
				receipts = "  + receipts"
			}
			fmt.Fprintf(a.out, "  %-34s %8.1f KiB%s\n", filepath.Base(p), float64(size)/1024, receipts)
		}
		return nil
	}

	// No lock: VACUUM INTO reads one consistent instant through SQLite's own locking,
	// so a backup beside the running server is safe and is the normal case.
	if err := requireDB(dbPath); err != nil {
		return err
	}

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
		Dir: dir, UploadDir: uploadDir, Keep: keep, AcceptEmpty: acceptEmpty,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "Snapshot written and verified in %s\n", snap.Took.Round(time.Millisecond))
	fmt.Fprintf(a.out, "  %s  (%.1f KiB)\n", snap.Path, float64(snap.Bytes)/1024)
	if snap.Uploads != "" {
		fmt.Fprintf(a.out, "  %s\n", snap.Uploads)
	}
	for _, t := range []string{"users", "households", "transactions", "funds"} {
		fmt.Fprintf(a.out, "  %-18s %d\n", t, snap.Counts[t])
	}
	if len(snap.Pruned) > 0 {
		fmt.Fprintf(a.out, "Pruned %d old file(s), keeping the newest %d snapshots.\n",
			len(snap.Pruned), keep)
	}
	return nil
}

// runRestore puts a snapshot back after proving it is usable. It is the one command
// allowed to create the database, because restoring onto a fresh machine is what it
// is for.
func (a *app) runRestore(from, dir, dbPath string, check bool) error {
	if from == "" {
		found, err := db.Snapshots(dir)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no snapshots found in %s (pass -from, or -dir / YABA_BACKUP_DIR)", dir)
		}
		from = found[len(found)-1]
		fmt.Fprintf(a.out, "Using the newest snapshot: %s\n", from)
	}

	if check {
		counts, err := db.VerifySnapshot(context.Background(), from)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s verifies.\n", filepath.Base(from))
		for _, t := range []string{"users", "households", "transactions", "funds"} {
			fmt.Fprintf(a.out, "  %-18s %d\n", t, counts[t])
		}
		return nil
	}

	// The directory has to exist already: creating it would hide a mistyped -db
	// behind a restore into a brand new, unexpected place.
	if info, err := os.Stat(filepath.Dir(dbPath)); err != nil || !info.IsDir() {
		return fmt.Errorf("the directory for %s does not exist; create it first or fix -db / YABA_DB", dbPath)
	}

	lock, err := lockDB(dbPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(a.out, "There is no database at %s yet; the restore will create it.\n", dbPath)
	}

	// Whatever is there is preserved before being replaced: a restore is done under
	// pressure, and the file just replaced sometimes turns out to have been the good
	// one. db.Restore checkpoints it and moves it aside with its -wal and -shm, so
	// commits the server had not yet checkpointed travel with it.
	aside, err := db.Restore(context.Background(), from, dbPath, true)
	if err != nil {
		return err
	}
	if len(aside) > 0 {
		fmt.Fprintf(a.out, "Moved the existing database to %s\n", aside[0])
		for _, p := range aside[1:] {
			fmt.Fprintf(a.out, "  with %s\n", p)
		}
	}
	fmt.Fprintf(a.out, "Restored %s to %s\n", filepath.Base(from), dbPath)
	fmt.Fprintf(a.out, "Receipts are not restored automatically: unzip the matching "+
		"%s-uploads.zip over your uploads directory if you need them.\n",
		strings.TrimSuffix(filepath.Base(from), ".db"))
	return nil
}

// clearDir empties a directory without removing the directory itself.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
