// Tests for snapshot ordering, the emptied-table guard, the backup schedule, the
// default backup directory and the database lock.
package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
)

func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func bases(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// ── ordering ──────────────────────────────────────────────────────────────────

func TestParseSnapshotName(t *testing.T) {
	stamp := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		wantOK  bool
		wantSeq int
	}{
		{"yaba-20260801-030000Z.db", true, 1},
		{"yaba-20260801-030000Z-2.db", true, 2},
		{"yaba-20260801-030000Z-10.db", true, 10},
		{"yaba-20260801-030000Z-1.db", false, 0}, // freeSnapshotPath never writes -1
		{"yaba-20260801-030000Z-x.db", false, 0},
		{"yaba-20260801-030000Z2.db", false, 0},
		{"yaba-20260801-030000Z-uploads.zip", false, 0},
		{"yaba-notes.db", false, 0},
		{"pre-empty-yaba-20260801-030000Z.db", false, 0},
		{"yaba.db", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, seq, ok := db.ParseSnapshotName(tt.name)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (!at.Equal(stamp) || seq != tt.wantSeq) {
				t.Errorf("got (%s, %d), want (%s, %d)", at, seq, stamp, tt.wantSeq)
			}
		})
	}
}

// TestSameSecondSnapshotsSortInWriteOrder is the ordering bug: '-' sorts before '.',
// so lexically "…Z-2.db" came before the "…Z.db" written a moment earlier, and prune,
// the truncation baseline and "restore the newest" all picked the older file.
func TestSameSecondSnapshotsSortInWriteOrder(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir,
		"yaba-20260801-030001Z.db",
		"yaba-20260801-030000Z-10.db",
		"yaba-20260801-030000Z-2.db",
		"yaba-20260801-030000Z.db",
		"yaba-20260731-235959Z-3.db",
		"yaba-notes.db", // not a name Backup writes: never listed, never pruned
	)
	found, err := db.Snapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"yaba-20260731-235959Z-3.db",
		"yaba-20260801-030000Z.db",
		"yaba-20260801-030000Z-2.db",
		"yaba-20260801-030000Z-10.db",
		"yaba-20260801-030001Z.db",
	}
	if got := bases(found); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order:\n got %v\nwant %v", got, want)
	}

	if _, err := db.Prune(dir, 2); err != nil {
		t.Fatal(err)
	}
	left, _ := db.Snapshots(dir)
	if got := bases(left); strings.Join(got, " ") != "yaba-20260801-030000Z-10.db yaba-20260801-030001Z.db" {
		t.Errorf("prune kept the wrong snapshots: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "yaba-notes.db")); err != nil {
		t.Error("prune removed a file that is not a snapshot")
	}
}

// TestRapidBackupsKeepTheNewest drives the same bug through Backup and retention:
// the newest snapshot must survive however many land in one second.
func TestRapidBackupsKeepTheNewest(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()
	var last string
	for i := 0; i < 3; i++ {
		snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir, Keep: 1})
		if err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		last = snap.Path
	}
	left, _ := db.Snapshots(dir)
	if len(left) != 1 || left[0] != last {
		t.Errorf("keep=1 after three rapid backups left %v, want only %s", bases(left), filepath.Base(last))
	}
}

// ── the emptied-table guard ──────────────────────────────────────────────────

// emptyAfterFirstBackup takes one good backup, then deletes every user.
func emptyAfterFirstBackup(t *testing.T) (dir string, backup func(db.BackupConfig) (db.Snapshot, error)) {
	t.Helper()
	sqlDB, _ := newDB(t)
	dir = t.TempDir()
	if _, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir}); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM users`); err != nil {
		t.Fatalf("delete users: %v", err)
	}
	return dir, func(cfg db.BackupConfig) (db.Snapshot, error) {
		cfg.Dir = dir
		return db.Backup(context.Background(), sqlDB, cfg)
	}
}

func preEmptyFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "pre-empty-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEmptiedTablesAreAcceptedOnceSeenConsistently(t *testing.T) {
	_, backup := emptyAfterFirstBackup(t)
	// The window is varied rather than slept through, so the test does not depend on
	// how long a backup takes (it is much slower under -race).
	cfg := db.BackupConfig{ConfirmEmptyAfter: time.Hour}

	// First sighting: refused, with the way out spelled out.
	_, err := backup(cfg)
	var emptied *db.EmptiedError
	if !errors.As(err, &emptied) {
		t.Fatalf("want an EmptiedError, got %v", err)
	}
	for _, want := range []string{"truncated", "users", "accept-empty", db.AcceptEmptyEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if len(emptied.Tables) < 2 {
		t.Errorf("deleting every user empties several guarded tables, got %v", emptied.Tables)
	}

	// Still inside the window: refused again.
	if _, err := backup(cfg); err == nil {
		t.Fatal("accepted before the emptied state had persisted")
	}

	// Still the same once the window has passed: this is the database's real state.
	cfg.ConfirmEmptyAfter = time.Nanosecond
	snap, err := backup(cfg)
	if err != nil {
		t.Fatalf("a consistently empty database must eventually back up: %v", err)
	}
	if snap.Counts["users"] != 0 {
		t.Errorf("snapshot has %d users, want 0", snap.Counts["users"])
	}

	// And from now on it is the baseline, so backups do not keep failing.
	if _, err := backup(db.BackupConfig{}); err != nil {
		t.Fatalf("the backup after acceptance failed: %v", err)
	}
}

// TestAcceptingAnEmptiedStateKeepsTheLastFullSnapshot: accepting must not let
// retention rotate away the only copy of the deleted rows.
func TestAcceptingAnEmptiedStateKeepsTheLastFullSnapshot(t *testing.T) {
	dir, backup := emptyAfterFirstBackup(t)
	if _, err := backup(db.BackupConfig{AcceptEmpty: true}); err != nil {
		t.Fatalf("-accept-empty should accept at once: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := backup(db.BackupConfig{Keep: 1}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	kept := preEmptyFiles(t, dir)
	if len(kept) != 1 {
		t.Fatalf("want one pre-empty snapshot preserved, got %v", kept)
	}
	counts, err := db.VerifySnapshot(context.Background(), kept[0])
	if err != nil {
		t.Fatalf("the preserved snapshot does not verify: %v", err)
	}
	if counts["users"] != 1 {
		t.Errorf("the preserved snapshot holds %d users, want the 1 deleted", counts["users"])
	}
}

func TestAcceptEmptyEnvironmentVariable(t *testing.T) {
	_, backup := emptyAfterFirstBackup(t)
	t.Setenv(db.AcceptEmptyEnv, "nonsense")
	if _, err := backup(db.BackupConfig{}); err == nil {
		t.Fatal("an unparseable value must not switch the guard off")
	}
	t.Setenv(db.AcceptEmptyEnv, "true")
	if _, err := backup(db.BackupConfig{}); err != nil {
		t.Fatalf("%s=true should accept: %v", db.AcceptEmptyEnv, err)
	}
}

// ── the schedule ──────────────────────────────────────────────────────────────

func TestFirstBackupDelay(t *testing.T) {
	restore := db.SetBackupTiming(time.Minute, time.Hour)
	defer restore()

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	every := 24 * time.Hour
	tests := []struct {
		name   string
		newest string // "" for an empty directory
		want   time.Duration
	}{
		{"no snapshot yet", "", time.Minute},
		{"overdue", "yaba-20260731-120000Z.db", time.Minute},
		{"almost due", "yaba-20260801-115930Z.db", time.Minute},
		{"taken 2h ago", "yaba-20260802-100000Z.db", 22 * time.Hour},
		{"same-second suffix is still read", "yaba-20260802-100000Z-2.db", 22 * time.Hour},
		{"stamped in the future", "yaba-20260810-000000Z.db", every},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.newest != "" {
				touch(t, dir, "yaba-20260701-000000Z.db", tt.newest)
			}
			if got := db.FirstBackupDelay(dir, every, now); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestBackupLoopBacksUpSoonAfterStart: the loop used to wait a full interval, so a
// server restarted daily never took a scheduled backup.
func TestBackupLoopBacksUpSoonAfterStart(t *testing.T) {
	restore := db.SetBackupTiming(10*time.Millisecond, 10*time.Millisecond)
	defer restore()

	sqlDB, _ := newDB(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		db.BackupLoop(ctx, sqlDB, db.BackupConfig{Dir: dir}, 24*time.Hour)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found, _ := db.Snapshots(dir); len(found) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backup was taken shortly after the loop started")
}

// ── the default directory ─────────────────────────────────────────────────────

func TestBackupDirFor(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "data", "yaba.db")
	tests := []struct {
		dbPath, want string
	}{
		{abs, filepath.Join(filepath.Dir(abs), "backups")},
		{"yaba.db", filepath.Join(wd, "backups")},
		{filepath.Join("sub", "yaba.db"), filepath.Join(wd, "sub", "backups")},
	}
	for _, tt := range tests {
		if got := db.BackupDirFor(tt.dbPath); got != tt.want {
			t.Errorf("BackupDirFor(%q) = %q, want %q", tt.dbPath, got, tt.want)
		}
	}
}

func TestDefaultBackupDirFollowsYABA_DB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yaba.db")
	t.Setenv("YABA_DB", path)
	if got, want := db.DefaultBackupDir(), filepath.Join(filepath.Dir(path), "backups"); got != want {
		t.Errorf("DefaultBackupDir() = %q, want %q", got, want)
	}
	t.Setenv("YABA_DB", "")
	if got := db.DefaultBackupDir(); got != db.BackupDirFor("yaba.db") {
		t.Errorf("with YABA_DB unset, DefaultBackupDir() = %q", got)
	}
}

// ── the lock ──────────────────────────────────────────────────────────────────

func TestLockIsExclusiveAndReleasable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "yaba.db")

	first, err := db.AcquireLock(dbPath)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	// A second holder is refused, even from the same process: that is what lets the
	// CLI tests stand in for a running server.
	if _, err := db.AcquireLock(dbPath); !errors.Is(err, db.ErrLocked) {
		t.Fatalf("second lock: want ErrLocked, got %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Errorf("a second Release should be a no-op: %v", err)
	}

	again, err := db.AcquireLock(dbPath)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again.Release()

	var none *db.Lock
	if err := none.Release(); err != nil {
		t.Errorf("Release on nil: %v", err)
	}
	if _, err := os.Stat(db.LockPath(dbPath)); err != nil {
		t.Errorf("the lock file should remain for the next holder: %v", err)
	}
}

func TestLockInAMissingDirectoryFails(t *testing.T) {
	_, err := db.AcquireLock(filepath.Join(t.TempDir(), "nope", "yaba.db"))
	if err == nil || errors.Is(err, db.ErrLocked) {
		t.Errorf("want a plain error for a missing directory, got %v", err)
	}
}
