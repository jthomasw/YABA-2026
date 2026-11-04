package db_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// Checking a backup name that doesn't exist must not create an empty file
// that then passes for the newest snapshot.
func TestVerifyMissingSnapshotCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "yaba-20260801-030000Z.db")
	if _, err := db.VerifySnapshot(context.Background(), p); err == nil {
		t.Fatal("a missing snapshot verified")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("verifying a missing path created %s", p)
	}
	empty := filepath.Join(dir, "yaba-20260802-030000Z.db")
	os.WriteFile(empty, nil, 0o600)
	if _, err := db.VerifySnapshot(context.Background(), empty); err == nil {
		t.Error("an empty file verified as a snapshot")
	}
}

// A snapshot made by a newer build is refused rather than restored.
func TestSnapshotFromNewerVersionIsRefused(t *testing.T) {
	sqlDB, path := newDB(t)
	if _, err := sqlDB.Exec(`INSERT INTO schema_migrations(version, name) VALUES(?, 'from the future')`,
		db.LatestVersion()+5); err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()
	_, err := db.VerifySnapshot(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("newer snapshot: err = %v", err)
	}
}

// A receipt that disappears while receipts are being archived is left out,
// instead of failing the whole backup. A dangling link stands in for a file
// deleted between the folder listing and the read.
func TestReceiptGoneMidBackupIsSkipped(t *testing.T) {
	up := t.TempDir()
	os.MkdirAll(filepath.Join(up, "1"), 0o700)
	os.WriteFile(filepath.Join(up, "1", "kept.jpg"), []byte("jpeg"), 0o600)
	if err := os.Symlink(filepath.Join(up, "1", "gone.jpg"), filepath.Join(up, "1", "vanished.jpg")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	n, err := db.ArchiveUploads(up, filepath.Join(t.TempDir(), "r.zip"))
	if err != nil || n != 1 {
		t.Fatalf("archive: stored %d, err %v; want 1 and no error", n, err)
	}
	if _, files, err := db.UploadsFingerprint(up); err != nil || files < 1 {
		t.Fatalf("fingerprint: %d files, err %v", files, err)
	}
}
