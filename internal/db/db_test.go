// Tests for the backup, verification and restore path.
//
// The point of these is narrow and worth stating: a backup is a claim about the
// future, and the only way to test a claim about restoring is to restore. So
// most of what follows writes a real snapshot of a real migrated database and
// then reads it back, rather than asserting that the right functions were
// called.
package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// newDB returns an open, migrated database with one user, one household and a
// handful of transactions -- enough that the invariants VerifySnapshot checks
// have something to be true about.
func newDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yaba.db")

	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seed(t, sqlDB, "a@example.com")
	return sqlDB, path
}

// seed adds one account with its personal household and three transactions.
func seed(t *testing.T, sqlDB *sql.DB, email string) int64 {
	t.Helper()

	res, err := sqlDB.Exec(
		`INSERT INTO users(username, email, display_name, password_hash)
		 VALUES(?, ?, 'Test', 'hash')`, email, email)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()

	res, err = sqlDB.Exec(
		`INSERT INTO households(name, personal_for) VALUES('My budget', ?)`, uid)
	if err != nil {
		t.Fatalf("insert household: %v", err)
	}
	hid, _ := res.LastInsertId()

	if _, err := sqlDB.Exec(
		`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`,
		hid, uid); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	if _, err := sqlDB.Exec(`UPDATE users SET active_household_id = ? WHERE id = ?`,
		hid, uid); err != nil {
		t.Fatalf("set active household: %v", err)
	}

	for i := 0; i < 3; i++ {
		addTransaction(t, sqlDB, uid, hid, i)
	}
	return uid
}

func addTransaction(t *testing.T, sqlDB *sql.DB, uid, hid int64, n int) {
	t.Helper()
	if _, err := sqlDB.Exec(`
		INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on)
		VALUES(?, ?, 'income', ?, ?, '2026-08-01')`,
		uid, hid, fmt.Sprintf("pay %d", n), 1000+n); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
}

func count(t *testing.T, sqlDB *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ── the whole round trip ──────────────────────────────────────────────────────

// TestBackupCanActuallyBeRestored is the test that justifies the feature. A
// snapshot nobody has restored is a hypothesis.
func TestBackupCanActuallyBeRestored(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if snap.Counts["transactions"] != 3 {
		t.Errorf("snapshot recorded %d transactions, want 3", snap.Counts["transactions"])
	}

	// Restore somewhere else and read it as a live database.
	restored := filepath.Join(t.TempDir(), "restored.db")
	if _, err := db.Restore(context.Background(), snap.Path, restored, false); err != nil {
		t.Fatalf("restore: %v", err)
	}

	reopened, err := db.Open(restored)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer reopened.Close()

	if got := count(t, reopened, "transactions"); got != 3 {
		t.Errorf("restored database has %d transactions, want 3", got)
	}
	if got := count(t, reopened, "users"); got != 1 {
		t.Errorf("restored database has %d users, want 1", got)
	}

	// And it is a working database, not just a readable one: migrations report
	// as up to date rather than trying to run again.
	pending, err := db.Pending(reopened)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("restored database claims %d pending migrations, want 0", pending)
	}
}

// TestSnapshotSeesCommittedWritesOnly is the reason VACUUM INTO is used instead
// of copying the file: the snapshot must be one consistent instant, and it must
// include everything committed before it.
func TestSnapshotSeesCommittedWritesOnly(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	// Write more rows, then snapshot. With WAL these commits are in the sidecar,
	// which is exactly the state a naive file copy loses.
	var uid, hid int64
	if err := sqlDB.QueryRow(
		`SELECT id, active_household_id FROM users LIMIT 1`).Scan(&uid, &hid); err != nil {
		t.Fatalf("read seed ids: %v", err)
	}
	for i := 0; i < 20; i++ {
		addTransaction(t, sqlDB, uid, hid, 100+i)
	}

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := snap.Counts["transactions"]; got != 23 {
		t.Errorf("snapshot has %d transactions, want 23 — commits in the WAL were lost", got)
	}
}

// TestSnapshotDuringConcurrentWritesIsSound takes a backup while another
// goroutine is writing. The count may legitimately land anywhere in the range,
// but the snapshot must be internally consistent either way.
func TestSnapshotDuringConcurrentWritesIsSound(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	var uid, hid int64
	if err := sqlDB.QueryRow(
		`SELECT id, active_household_id FROM users LIMIT 1`).Scan(&uid, &hid); err != nil {
		t.Fatalf("read seed ids: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			sqlDB.Exec(`
				INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on)
				VALUES(?, ?, 'income', 'concurrent', 500, '2026-08-02')`, uid, hid)
		}
	}()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("backup during writes: %v", err)
	}
	// Backup verifies internally, so reaching here means integrity_check and
	// foreign_key_check both passed. Assert the count is at least the committed
	// baseline rather than a specific value.
	if snap.Counts["transactions"] < 3 {
		t.Errorf("snapshot has %d transactions, want at least the 3 committed before it",
			snap.Counts["transactions"])
	}
}

// ── verification actually rejects things ──────────────────────────────────────

// TestVerifyRejectsCorruption is the check the whole feature exists for. If
// integrity_check is never exercised against a genuinely damaged file, there is
// no evidence it would catch one.
func TestVerifyRejectsCorruption(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Overwrite a stretch of the middle of the file, past the header, so the
	// damage is in the b-tree pages rather than something SQLite refuses at open.
	f, err := os.OpenFile(snap.Path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open snapshot for damage: %v", err)
	}
	junk := make([]byte, 2048)
	for i := range junk {
		junk[i] = 0xA5
	}
	if _, err := f.WriteAt(junk, 4096); err != nil {
		t.Fatalf("damage snapshot: %v", err)
	}
	f.Close()

	if _, err := db.VerifySnapshot(context.Background(), snap.Path); err == nil {
		t.Fatal("VerifySnapshot accepted a corrupted file")
	}

	// And a restore from it must refuse before touching the destination.
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Restore(context.Background(), snap.Path, target, true); err == nil {
		t.Fatal("Restore accepted a corrupted snapshot")
	}
	if got, _ := os.ReadFile(target); string(got) != "existing" {
		t.Error("a refused restore overwrote the destination anyway")
	}
}

// TestVerifyRejectsABrokenInvariant covers the case a snapshot is valid SQLite
// and still useless to this application.
func TestVerifyRejectsABrokenInvariant(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	// A household with no owner. Nothing in SQLite forbids it; YABA cannot use
	// it, because no one could ever administer that budget.
	if _, err := sqlDB.Exec(`INSERT INTO households(name) VALUES('Orphan')`); err != nil {
		t.Fatalf("insert household: %v", err)
	}

	_, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err == nil {
		t.Fatal("backup accepted a database with an unowned household")
	}
	if !strings.Contains(err.Error(), "no owner") {
		t.Errorf("error should name the broken invariant, got: %v", err)
	}

	// The unusable snapshot must not be left behind implying safety.
	found, _ := db.Snapshots(dir)
	if len(found) != 0 {
		t.Errorf("a rejected snapshot was left on disk: %v", found)
	}
}

// TestTruncatedSnapshotIsRefused is the monotonic-count guard: a snapshot whose
// guarded tables have gone empty is the signature of a broken backup.
func TestTruncatedSnapshotIsRefused(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	if _, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir}); err != nil {
		t.Fatalf("first backup: %v", err)
	}

	// Empty the database. Cascades take households, memberships and
	// transactions with the users.
	if _, err := sqlDB.Exec(`DELETE FROM users`); err != nil {
		t.Fatalf("delete users: %v", err)
	}

	_, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err == nil {
		t.Fatal("backup accepted a snapshot with no users when the previous one had some")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should say the snapshot looks truncated, got: %v", err)
	}

	// The good snapshot survives; the refused one does not.
	found, _ := db.Snapshots(dir)
	if len(found) != 1 {
		t.Errorf("want the one good snapshot to remain, found %d", len(found))
	}
}

// TestShrinkingIsAllowed is the other half of the same guard. `yaba reset -keep`
// deletes accounts deliberately, and a check that fired on every intentional
// deletion would be switched off within a week.
func TestShrinkingIsAllowed(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	seed(t, sqlDB, "b@example.com")
	if _, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir}); err != nil {
		t.Fatalf("first backup: %v", err)
	}

	if _, err := sqlDB.Exec(`DELETE FROM users WHERE email = 'b@example.com'`); err != nil {
		t.Fatalf("delete one user: %v", err)
	}

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("a deliberate deletion must not fail the backup: %v", err)
	}
	if snap.Counts["users"] != 1 {
		t.Errorf("snapshot has %d users, want 1", snap.Counts["users"])
	}
}

// ── retention ─────────────────────────────────────────────────────────────────

func TestPruneKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"yaba-20260801-030000Z.db",
		"yaba-20260802-030000Z.db",
		"yaba-20260803-030000Z.db",
		"yaba-20260804-030000Z.db",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One snapshot has a receipts archive, which must go with it.
	archive := filepath.Join(dir, "yaba-20260801-030000Z-uploads.zip")
	if err := os.WriteFile(archive, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unrelated file must be left alone.
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := db.Prune(dir, 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 3 { // two snapshots plus one archive
		t.Errorf("removed %d files, want 3: %v", len(removed), removed)
	}

	left, err := db.Snapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("want 2 snapshots left, got %d", len(left))
	}
	if filepath.Base(left[0]) != names[2] || filepath.Base(left[1]) != names[3] {
		t.Errorf("the wrong two survived: %v", left)
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("the receipts archive outlived its snapshot")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("prune deleted an unrelated file")
	}
}

// TestPruneNeverEmptiesTheDirectory: a retention policy that can delete
// everything is a deletion policy.
func TestPruneNeverEmptiesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"yaba-20260801-030000Z.db", "yaba-20260802-030000Z.db"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Prune(dir, 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	left, _ := db.Snapshots(dir)
	if len(left) != 1 {
		t.Errorf("keep=0 left %d snapshots, want 1", len(left))
	}
}

func TestRetentionRunsAsPartOfBackup(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	for i := 0; i < 4; i++ {
		if _, err := db.Backup(context.Background(), sqlDB,
			db.BackupConfig{Dir: dir, Keep: 2}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	left, _ := db.Snapshots(dir)
	if len(left) != 2 {
		t.Errorf("after four backups keeping 2, %d remain", len(left))
	}
}

// TestSnapshotsInTheSameSecondDoNotCollide: VACUUM INTO refuses to overwrite, so
// a pre-migration backup immediately followed by a manual one must still work.
func TestSnapshotsInTheSameSecondDoNotCollide(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	for i := 0; i < 3; i++ {
		if _, err := db.Backup(context.Background(), sqlDB,
			db.BackupConfig{Dir: dir, Keep: 10}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	left, _ := db.Snapshots(dir)
	if len(left) != 3 {
		t.Errorf("three rapid backups produced %d snapshots", len(left))
	}
}

// ── receipts ──────────────────────────────────────────────────────────────────

// TestUploadsTravelWithTheDatabase: receipts are files on disk, so a
// database-only backup restores a ledger whose attachments are all missing.
func TestUploadsTravelWithTheDatabase(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	uploads := t.TempDir()
	if err := os.MkdirAll(filepath.Join(uploads, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "7", "receipt.jpg"),
		[]byte("jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := db.Backup(context.Background(), sqlDB,
		db.BackupConfig{Dir: dir, UploadDir: uploads})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if snap.Uploads == "" {
		t.Fatal("receipts were not archived")
	}
	if _, err := os.Stat(snap.Uploads); err != nil {
		t.Errorf("archive is not on disk: %v", err)
	}
}

// TestNoUploadsMeansNoEmptyArchive keeps the backup directory free of noise.
func TestNoUploadsMeansNoEmptyArchive(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	snap, err := db.Backup(context.Background(), sqlDB,
		db.BackupConfig{Dir: dir, UploadDir: filepath.Join(t.TempDir(), "does-not-exist")})
	if err != nil {
		t.Fatalf("a missing uploads directory must not fail the backup: %v", err)
	}
	if snap.Uploads != "" {
		t.Errorf("archived %q from a directory that does not exist", snap.Uploads)
	}
}

// ── restore mechanics ─────────────────────────────────────────────────────────

// TestRestoreSetsStaleSidecarsAsideNotDeletes is the easy-to-miss step. A -wal left
// beside the replaced database must not be replayed into the restored one -- but it
// may hold commits nobody checkpointed, so it is moved with the file it belongs to,
// never deleted. Here the old "database" is junk, so the checkpoint cannot run and
// the fallback path is the one exercised.
func TestRestoreSetsStaleSidecarsAsideNotDeletes(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	target := filepath.Join(t.TempDir(), "yaba.db")
	if err := os.WriteFile(target, []byte("old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(target+s, []byte("stale"+s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	aside, err := db.Restore(context.Background(), snap.Path, target, true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(aside) != 3 {
		t.Fatalf("want the database and both sidecars set aside, got %v", aside)
	}
	if got, _ := os.ReadFile(aside[0]); string(got) != "old database" {
		t.Errorf("%s does not hold the replaced database", aside[0])
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(target + s); err == nil {
			t.Errorf("stale %s was left beside the restored database", s)
		}
		// The sidecar keeps its suffix relative to the set-aside name, so SQLite
		// would pair them up again if that file were opened.
		if got, err := os.ReadFile(aside[0] + s); err != nil || string(got) != "stale"+s {
			t.Errorf("%s was not preserved as %s%s: %v", s, aside[0], s, err)
		}
	}

	reopened, err := db.Open(target)
	if err != nil {
		t.Fatalf("restored database will not open: %v", err)
	}
	defer reopened.Close()
	if got := count(t, reopened, "transactions"); got != 3 {
		t.Errorf("restored database has %d transactions, want 3", got)
	}
}

// crashImage leaves at a new path what a server killed mid-run leaves on disk: a
// database whose most recent commits exist only in its -wal. It returns the path and
// the number of transactions committed, of which extra are in the WAL alone.
func crashImage(t *testing.T, extra int) (string, int64) {
	t.Helper()
	sqlDB, livePath := newDB(t)
	if _, err := sqlDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := sqlDB.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatalf("disable autocheckpoint: %v", err)
	}
	var uid, hid int64
	if err := sqlDB.QueryRow(`SELECT id, active_household_id FROM users LIMIT 1`).Scan(&uid, &hid); err != nil {
		t.Fatalf("read seed ids: %v", err)
	}
	for i := 0; i < extra; i++ {
		addTransaction(t, sqlDB, uid, hid, 500+i)
	}

	// Copy while the connection is still open and idle: every frame is a complete
	// commit, and closing it would checkpoint them away.
	path := filepath.Join(t.TempDir(), "yaba.db")
	for _, s := range []string{"", "-wal"} {
		raw, err := os.ReadFile(livePath + s)
		if err != nil {
			t.Fatalf("read %s%s: %v", livePath, s, err)
		}
		if s == "-wal" && len(raw) == 0 {
			t.Fatal("the WAL is empty, so this test would prove nothing")
		}
		if err := os.WriteFile(path+s, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path, 3 + int64(extra)
}

// TestRestoreKeepsUncheckpointedCommits is the bug this guards against: restore used to
// move only the main file aside and delete the -wal, so the "before-restore" copy
// silently lacked every commit since the last checkpoint.
func TestRestoreKeepsUncheckpointedCommits(t *testing.T) {
	sqlDB, _ := newDB(t)
	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	target, want := crashImage(t, 5)

	aside, err := db.Restore(context.Background(), snap.Path, target, true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The checkpoint folded the WAL in, so the main file is complete on its own.
	if len(aside) != 1 {
		t.Errorf("checkpoint should have left no sidecars to move, got %v", aside)
	}
	before, err := db.Open(aside[0])
	if err != nil {
		t.Fatalf("open set-aside database: %v", err)
	}
	defer before.Close()
	if got := count(t, before, "transactions"); got != want {
		t.Errorf("set-aside database has %d transactions, want %d — WAL commits were lost", got, want)
	}
}

// TestRestoreMovesTheWALWhenItCannotCheckpoint covers the fallback: a reader holding a
// snapshot stops the checkpoint completing, so the -wal must travel with the file.
func TestRestoreMovesTheWALWhenItCannotCheckpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows locks open files; rename-while-open is Linux-only behaviour")
	}
	sqlDB, _ := newDB(t)
	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	target, want := crashImage(t, 5)

	reader, err := db.Open(target)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	tx, err := reader.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}

	aside, err := db.Restore(context.Background(), snap.Path, target, true)
	tx.Rollback()
	reader.Close()
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(aside) < 2 || aside[1] != aside[0]+"-wal" {
		t.Fatalf("the -wal should have been moved with the database, got %v", aside)
	}

	before, err := db.Open(aside[0])
	if err != nil {
		t.Fatalf("open set-aside database: %v", err)
	}
	defer before.Close()
	if got := count(t, before, "transactions"); got != want {
		t.Errorf("set-aside database has %d transactions, want %d", got, want)
	}
}

// TestRestoreWillNotOverwriteWithoutForce guards against a restore drill
// destroying the live database by accident.
func TestRestoreWillNotOverwriteWithoutForce(t *testing.T) {
	sqlDB, _ := newDB(t)
	dir := t.TempDir()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	target := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Restore(context.Background(), snap.Path, target, false); err == nil {
		t.Fatal("restore overwrote an existing database without -force")
	}
	if got, _ := os.ReadFile(target); string(got) != "precious" {
		t.Error("the existing database was modified despite the refusal")
	}
}

// ── configuration and plumbing ────────────────────────────────────────────────

func TestBackupNeedsADirectory(t *testing.T) {
	sqlDB, _ := newDB(t)
	if _, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{Dir: ""}); err == nil {
		t.Fatal("backup ran with no directory configured")
	}
}

// TestPendingDrivesThePreMigrationBackup: the startup hook only takes a snapshot
// when the schema is about to change, so this has to report honestly.
func TestPendingDrivesThePreMigrationBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	before, err := db.Pending(sqlDB)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if before == 0 {
		t.Error("a fresh database reports no pending migrations")
	}

	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after, err := db.Pending(sqlDB)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if after != 0 {
		t.Errorf("after migrating, %d still pending", after)
	}
}

// TestSnapshotsAreListedOldestFirst: retention deletes from the front of this
// list, so the ordering is load-bearing.
func TestSnapshotsAreListedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	// Written out of order on purpose.
	for _, n := range []string{
		"yaba-20260803-030000Z.db",
		"yaba-20260801-030000Z.db",
		"yaba-20260802-235959Z.db",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := db.Snapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"yaba-20260801-030000Z.db",
		"yaba-20260802-235959Z.db",
		"yaba-20260803-030000Z.db",
	}
	for i, w := range want {
		if filepath.Base(found[i]) != w {
			t.Errorf("position %d is %s, want %s", i, filepath.Base(found[i]), w)
		}
	}
}

func TestSnapshotsOfAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	found, err := db.Snapshots(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("listing a missing directory should be empty, not an error: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found %d snapshots in a directory that does not exist", len(found))
	}
}

// TestBackupLoopStopsWithItsContext: the loop shares the worker's cancellation,
// so a leak here would keep the process alive after shutdown.
func TestBackupLoopStopsWithItsContext(t *testing.T) {
	sqlDB, _ := newDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		db.BackupLoop(ctx, sqlDB, db.BackupConfig{Dir: t.TempDir()}, time.Hour)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("BackupLoop did not return when its context was cancelled")
	}
}
