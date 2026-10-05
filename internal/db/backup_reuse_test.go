package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// TestUnchangedReceiptsAreNotArchivedAgain: two backups with the same receipts
// share one archive on disk; a new receipt gets a new archive.
func TestUnchangedReceiptsAreNotArchivedAgain(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()
	uploads := t.TempDir()
	os.MkdirAll(filepath.Join(uploads, "1"), 0o700)
	os.WriteFile(filepath.Join(uploads, "1", "a.jpg"), []byte("receipt a"), 0o600)
	cfg := db.BackupConfig{Dir: dir, UploadDir: uploads, Keep: 10}

	backup := func() db.Snapshot {
		t.Helper()
		snap, err := db.Backup(context.Background(), sqlDB, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Uploads == "" {
			t.Fatal("no receipt archive was written")
		}
		time.Sleep(1100 * time.Millisecond) // a new snapshot name
		return snap
	}
	same := func(a, b string) bool {
		fa, _ := os.Stat(a)
		fb, _ := os.Stat(b)
		return fa != nil && fb != nil && os.SameFile(fa, fb)
	}

	first, second := backup(), backup()
	if !same(first.Uploads, second.Uploads) {
		t.Error("an unchanged receipt directory was zipped again instead of shared")
	}

	os.WriteFile(filepath.Join(uploads, "1", "b.jpg"), []byte("receipt b"), 0o600)
	third := backup()
	if same(second.Uploads, third.Uploads) {
		t.Error("a new receipt did not produce a new archive")
	}

	// Pruning one name of a shared archive must leave the other intact.
	os.Remove(first.Uploads)
	if _, err := os.Stat(second.Uploads); err != nil {
		t.Errorf("removing one snapshot's archive broke the other: %v", err)
	}
}

// TestBackupThroughAReadOnlyHandleDoesNotWaitForWriters: the server takes its
// snapshots through OpenReadOnly so a backup and a write never queue behind
// each other on the server's single connection.
func TestBackupThroughAReadOnlyHandleDoesNotWaitForWriters(t *testing.T) {
	sqlDB, path := newDB(t)
	ro, err := db.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET display_name = 'mid-write'`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.Backup(context.Background(), ro, db.BackupConfig{Dir: t.TempDir()})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the backup waited for an open write transaction")
	}
}
