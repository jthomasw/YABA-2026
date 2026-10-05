// Tests for the subcommands themselves, driven through run() the way a person at a
// terminal would: real databases in temp directories, typed confirmations on stdin,
// and the exit status checked rather than assumed.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/db"
)

type result struct {
	code        int
	out, errOut string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.out, r.errOut)
}

// yaba runs one command with the given environment and stdin. The environment is a
// map rather than the process's own, so nothing leaks in from the machine running
// the tests.
func yaba(t *testing.T, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	var out, errOut strings.Builder
	code := run(args, strings.NewReader(stdin), &out, &errOut,
		func(k string) string { return env[k] })
	return result{code, out.String(), errOut.String()}
}

// site is one deployment's worth of paths, laid out like /var/lib/yaba.
type site struct {
	dir, db, uploads string
}

// newSite creates a migrated database holding one account, its household, a fund,
// some income and one receipt, then closes it, as a stopped server would leave it.
func newSite(t *testing.T) site {
	t.Helper()
	dir := t.TempDir()
	s := site{dir: dir, db: filepath.Join(dir, "yaba.db"), uploads: filepath.Join(dir, "uploads")}

	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedUser(t, sqlDB, 1, "a@example.com")
	if _, err := sqlDB.Exec(`
		INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on)
		VALUES(1, 1, 'income', 'pay', 100000, '2026-01-01'),
		      (1, 1, 'expense', 'food', 2000, '2026-01-02');`); err != nil {
		t.Fatalf("seed transactions: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(s.uploads, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.uploads, "1", "r.jpg"), []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedUser(t *testing.T, sqlDB *sql.DB, id int, email string) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id, username, email, display_name, password_hash) VALUES(?, ?, ?, 'T', 'hash')`,
			[]any{id, email, email}},
		{`INSERT INTO households(id, name, personal_for) VALUES(?, 'Mine', ?)`, []any{id, id}},
		{`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`, []any{id, id}},
		{`INSERT INTO funds(user_id, household_id, name) VALUES(?, ?, 'Rainy day')`, []any{id, id}},
	} {
		if _, err := sqlDB.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed user %s: %v", email, err)
		}
	}
}

// env is the server's environment file for this site.
func (s site) env() map[string]string {
	return map[string]string{"YABA_DB": s.db, "YABA_UPLOADS": s.uploads}
}

func (s site) backups() string { return filepath.Join(s.dir, "backups") }

func (s site) count(t *testing.T, table string) int64 {
	t.Helper()
	return countIn(t, s.db, table)
}

func countIn(t *testing.T, path, table string) int64 {
	t.Helper()
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer sqlDB.Close()
	var n int64
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatalf("count %s in %s: %v", table, path, err)
	}
	return n
}

func mustSucceed(t *testing.T, r result) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("want success, got %s", r)
	}
}

func mustFail(t *testing.T, r result, code int, wantErr string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("want exit %d, got %s", code, r)
	}
	if !strings.Contains(r.errOut, wantErr) {
		t.Errorf("stderr should contain %q:\n%s", wantErr, r.errOut)
	}
}

// ── usage ─────────────────────────────────────────────────────────────────────

func TestUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"no subcommand", nil, 2},
		{"unknown subcommand", []string{"frobnicate"}, 2},
		{"help", []string{"help"}, 0},
		{"subcommand help", []string{"backup", "-h"}, 0},
		{"bad flag", []string{"backup", "-nope"}, 2},
		// "yaba backup list" must not quietly take a backup instead of listing.
		{"stray argument", []string{"backup", "list"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := yaba(t, nil, "", tt.args...); r.code != tt.code {
				t.Errorf("want exit %d, got %s", tt.code, r)
			}
		})
	}
}

// ── refusing a database that is not there ─────────────────────────────────────

// TestMissingDatabaseIsRefusedNotCreated: run from the wrong directory, the relative
// default used to create an empty yaba.db and then fail confusingly or report that
// there was nothing to do.
func TestMissingDatabaseIsRefusedNotCreated(t *testing.T) {
	for _, args := range [][]string{
		{"reset", "-yes"},
		{"passwd", "-list"},
		{"passwd", "-email", "a@example.com"},
		{"backup"},
		{"repair"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "yaba.db")
			r := yaba(t, map[string]string{"YABA_DB": missing}, "", args...)
			mustFail(t, r, 1, "no database at "+missing)
			if !strings.Contains(r.errOut, "YABA_DB") {
				t.Errorf("the message should say how to point at the right database:\n%s", r.errOut)
			}
			if _, err := os.Stat(missing); err == nil {
				t.Error("the command created the database it refused")
			}
			if _, err := os.Stat(db.LockPath(missing)); err == nil {
				t.Error("the command left a lock file beside a database that does not exist")
			}
		})
	}
}

func TestRelativeMissingDatabaseNamesTheResolvedPath(t *testing.T) {
	t.Chdir(t.TempDir())
	r := yaba(t, nil, "", "passwd", "-list")
	wd, _ := os.Getwd()
	mustFail(t, r, 1, filepath.Join(wd, "yaba.db"))
	if _, err := os.Stat("yaba.db"); err == nil {
		t.Error("an empty yaba.db was created in the current directory")
	}
}

func TestADirectoryIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	mustFail(t, yaba(t, nil, "", "backup", "-db", dir), 1, "is a directory")
}

// ── backup ────────────────────────────────────────────────────────────────────

func TestBackupUsesTheServersEnvironment(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "", "backup")
	mustSucceed(t, r)

	// Default directory: "backups" beside the database, as the server now uses.
	found, err := db.Snapshots(s.backups())
	if err != nil || len(found) != 1 {
		t.Fatalf("want one snapshot in %s, got %v (%v)\n%s", s.backups(), found, err, r)
	}
	if !strings.Contains(r.out, "verified") || !strings.Contains(r.out, found[0]) {
		t.Errorf("output should name the verified snapshot:\n%s", r.out)
	}
	zip := strings.TrimSuffix(found[0], ".db") + "-uploads.zip"
	if _, err := os.Stat(zip); err != nil {
		t.Errorf("receipts from YABA_UPLOADS were not archived: %v", err)
	}
}

func TestBackupDirectoryPrecedence(t *testing.T) {
	s := newSite(t)
	fromEnv := filepath.Join(t.TempDir(), "env-backups")
	fromFlag := filepath.Join(t.TempDir(), "flag-backups")
	env := s.env()
	env["YABA_BACKUP_DIR"] = fromEnv

	mustSucceed(t, yaba(t, env, "", "backup"))
	mustSucceed(t, yaba(t, env, "", "backup", "-dir", fromFlag))

	for dir, want := range map[string]int{fromEnv: 1, fromFlag: 1, s.backups(): 0} {
		if found, _ := db.Snapshots(dir); len(found) != want {
			t.Errorf("%s holds %d snapshot(s), want %d", dir, len(found), want)
		}
	}
}

func TestBackupKeepFromEnvironment(t *testing.T) {
	s := newSite(t)
	env := s.env()
	env["YABA_BACKUP_KEEP"] = "2"
	for i := 0; i < 3; i++ {
		mustSucceed(t, yaba(t, env, "", "backup"))
	}
	if found, _ := db.Snapshots(s.backups()); len(found) != 2 {
		t.Errorf("YABA_BACKUP_KEEP=2 left %d snapshots", len(found))
	}

	env["YABA_BACKUP_KEEP"] = "lots"
	r := yaba(t, env, "", "backup", "-list")
	mustSucceed(t, r)
	if !strings.Contains(r.errOut, "not a positive integer") {
		t.Errorf("a bad YABA_BACKUP_KEEP should be warned about:\n%s", r.errOut)
	}
}

func TestBackupList(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "", "backup", "-list")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "No snapshots in "+s.backups()) {
		t.Errorf("empty listing:\n%s", r.out)
	}

	mustSucceed(t, yaba(t, s.env(), "", "backup"))
	mustSucceed(t, yaba(t, s.env(), "", "backup"))
	r = yaba(t, s.env(), "", "backup", "-list")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "2 snapshot(s)") || strings.Count(r.out, "+ receipts") != 2 {
		t.Errorf("listing should show both snapshots with receipts:\n%s", r.out)
	}
}

// TestBackupAcceptEmpty: after the last account is deliberately deleted, the guard
// refuses, and -accept-empty is the operator's way through.
func TestBackupAcceptEmpty(t *testing.T) {
	s := newSite(t)
	mustSucceed(t, yaba(t, s.env(), "", "backup"))

	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()

	mustFail(t, yaba(t, s.env(), "", "backup"), 1, "-accept-empty")
	mustSucceed(t, yaba(t, s.env(), "", "backup", "-accept-empty"))
}

// ── restore ───────────────────────────────────────────────────────────────────

// snapshot takes a backup of the site and returns its path.
func (s site) snapshot(t *testing.T) string {
	t.Helper()
	mustSucceed(t, yaba(t, s.env(), "", "backup"))
	found, _ := db.Snapshots(s.backups())
	return found[len(found)-1]
}

func TestRestoreCheck(t *testing.T) {
	s := newSite(t)
	snap := s.snapshot(t)

	r := yaba(t, s.env(), "", "restore", "-check")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "Using the newest snapshot: "+snap) ||
		!strings.Contains(r.out, "verifies") {
		t.Errorf("-check output:\n%s", r.out)
	}

	bad := filepath.Join(t.TempDir(), "yaba-20260101-000000Z.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustFail(t, yaba(t, s.env(), "", "restore", "-check", "-from", bad), 1, "not a readable database")

	empty := t.TempDir()
	mustFail(t, yaba(t, s.env(), "", "restore", "-check", "-dir", empty), 1, "no snapshots found")
}

// TestRestorePreservesUncheckpointedCommits: the database being replaced may hold
// commits that exist only in its -wal (the server was killed, or is the reason for
// the restore). They must survive in the before-restore copy.
func TestRestorePreservesUncheckpointedCommits(t *testing.T) {
	s := newSite(t)
	s.snapshot(t) // 2 transactions

	// Add rows and leave them in the WAL alone, as a killed server would.
	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`PRAGMA wal_autocheckpoint = 0`,
		`INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on)
		 VALUES(1, 1, 'income', 'late', 500, '2026-02-01'), (1, 1, 'income', 'later', 500, '2026-02-02')`,
	} {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	crashed := filepath.Join(t.TempDir(), "yaba.db")
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(s.db + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(crashed+suffix, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sqlDB.Close()

	env := s.env()
	env["YABA_DB"] = crashed
	env["YABA_BACKUP_DIR"] = s.backups()
	r := yaba(t, env, "", "restore")
	mustSucceed(t, r)

	aside, _ := filepath.Glob(crashed + ".before-restore-*")
	var main string
	for _, p := range aside {
		if !strings.HasSuffix(p, "-wal") && !strings.HasSuffix(p, "-shm") {
			main = p
		}
	}
	if main == "" {
		t.Fatalf("no before-restore copy was made:\n%s", r)
	}
	if !strings.Contains(r.out, "Moved the existing database to "+main) {
		t.Errorf("output should say where the old database went:\n%s", r.out)
	}
	if got := countIn(t, main, "transactions"); got != 4 {
		t.Errorf("before-restore copy has %d transactions, want 4 — WAL commits were lost", got)
	}
	if got := countIn(t, crashed, "transactions"); got != 2 {
		t.Errorf("restored database has %d transactions, want the snapshot's 2", got)
	}
}

// TestRestoreCreatesAMissingDatabase: restore is the one command allowed to create
// the database, because restoring onto a new machine is what it is for.
func TestRestoreCreatesAMissingDatabase(t *testing.T) {
	s := newSite(t)
	snap := s.snapshot(t)

	target := filepath.Join(t.TempDir(), "yaba.db")
	r := yaba(t, map[string]string{"YABA_DB": target}, "", "restore", "-from", snap)
	mustSucceed(t, r)
	if !strings.Contains(r.out, "will create it") {
		t.Errorf("output should say the database is being created:\n%s", r.out)
	}
	if got := countIn(t, target, "users"); got != 1 {
		t.Errorf("restored database has %d users, want 1", got)
	}

	nowhere := filepath.Join(t.TempDir(), "missing-dir", "yaba.db")
	mustFail(t, yaba(t, nil, "", "restore", "-from", snap, "-db", nowhere), 1, "does not exist")
}

// TestGuardedCommandsRefuseWhileTheServerHoldsTheLock: restore, reset and repair
// replace or rewrite the database, which must not happen underneath a running server.
// The test holds the lock exactly as the server does.
func TestGuardedCommandsRefuseWhileTheServerHoldsTheLock(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"restore", []string{"restore"}, ""},
		{"reset", []string{"reset"}, "reset\n"},
		{"repair", []string{"repair"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t)
			s.snapshot(t)
			before, _ := os.ReadFile(s.db)

			server, err := db.AcquireLock(s.db)
			if err != nil {
				t.Fatalf("lock: %v", err)
			}
			defer server.Release()

			r := yaba(t, s.env(), tt.stdin, tt.args...)
			mustFail(t, r, 1, "in use by another process")
			if !strings.Contains(r.errOut, "systemctl stop") {
				t.Errorf("the refusal should say how to stop the server:\n%s", r.errOut)
			}
			if after, _ := os.ReadFile(s.db); string(after) != string(before) {
				t.Error("the database changed despite the refusal")
			}
			if aside, _ := filepath.Glob(s.db + ".before-restore-*"); len(aside) > 0 {
				t.Errorf("files were moved despite the refusal: %v", aside)
			}
		})
	}
}

// TestBackupAndPasswdRunBesideTheServer: these are safe under SQLite's own locking,
// and refusing them would make routine maintenance need downtime.
func TestBackupAndPasswdRunBesideTheServer(t *testing.T) {
	s := newSite(t)
	server, err := db.AcquireLock(s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Release()

	mustSucceed(t, yaba(t, s.env(), "", "backup"))
	mustSucceed(t, yaba(t, s.env(), "a new password\na new password\n", "passwd", "-email", "a@example.com"))
}

// ── reset ─────────────────────────────────────────────────────────────────────

func TestResetDeclined(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "nope\n", "reset")
	mustFail(t, r, 1, "cancelled")
	if !strings.Contains(r.out, "Current contents:") || !strings.Contains(r.out, "Type 'reset'") {
		t.Errorf("the prompt should follow a summary of what would go:\n%s", r.out)
	}
	if got := s.count(t, "transactions"); got != 2 {
		t.Errorf("a declined reset deleted data: %d transactions left", got)
	}
	if found, _ := db.Snapshots(s.backups()); len(found) != 0 {
		t.Errorf("a declined reset still took a backup: %v", found)
	}
}

func TestResetAccepted(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "reset\n", "reset")
	mustSucceed(t, r)

	if got := s.count(t, "users"); got != 0 {
		t.Errorf("%d users survived the reset", got)
	}
	if v := countIn(t, s.db, "schema_migrations"); v == 0 {
		t.Error("the schema was not rebuilt after the reset")
	}
	found, _ := db.Snapshots(s.backups())
	if len(found) != 1 || !strings.Contains(r.out, "Backed up to "+found[0]) {
		t.Fatalf("want a safety snapshot beside the database first, got %v:\n%s", found, r.out)
	}
	if got := countIn(t, found[0], "transactions"); got != 2 {
		t.Errorf("the safety snapshot has %d transactions, want 2", got)
	}
	if entries, _ := os.ReadDir(s.uploads); len(entries) != 0 {
		t.Errorf("uploads were not cleared: %v", entries)
	}
}

func TestResetKeepOneAccount(t *testing.T) {
	s := newSite(t)
	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	seedUser(t, sqlDB, 2, "b@example.com")
	sqlDB.Close()
	if err := os.MkdirAll(filepath.Join(s.uploads, "2"), 0o700); err != nil {
		t.Fatal(err)
	}

	mustFail(t, yaba(t, s.env(), "", "reset", "-keep", "nobody@example.com"), 1, "no account matches")

	r := yaba(t, s.env(), "prune\n", "reset", "-keep", "A@example.com")
	mustSucceed(t, r)
	if got := s.count(t, "users"); got != 1 {
		t.Errorf("want only the kept account, %d remain", got)
	}
	if got := s.count(t, "transactions"); got != 2 {
		t.Errorf("the kept account's transactions were touched: %d", got)
	}
	if _, err := os.Stat(filepath.Join(s.uploads, "2")); err == nil {
		t.Error("the deleted account's receipts are still on disk")
	}
	if _, err := os.Stat(filepath.Join(s.uploads, "1")); err != nil {
		t.Error("the kept account's receipts were removed")
	}
}

func TestResetWithoutBackupOrPrompt(t *testing.T) {
	s := newSite(t)
	mustSucceed(t, yaba(t, s.env(), "", "reset", "-yes", "-backup=false"))
	if found, _ := db.Snapshots(s.backups()); len(found) != 0 {
		t.Errorf("-backup=false still took a snapshot: %v", found)
	}
	if got := s.count(t, "users"); got != 0 {
		t.Errorf("%d users survived", got)
	}
}

// ── passwd ────────────────────────────────────────────────────────────────────

func passwordHash(t *testing.T, s site) string {
	t.Helper()
	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var h string
	if err := sqlDB.QueryRow(`SELECT password_hash FROM users WHERE id = 1`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPasswdSetsThePasswordAndSignsOut(t *testing.T) {
	s := newSite(t)
	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO sessions(id, user_id, expires_at)
		VALUES('s1', 1, '2099-01-01'), ('s2', 1, '2099-01-01')`); err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()

	r := yaba(t, s.env(), "correct horse\ncorrect horse\n", "passwd", "-email", "A@Example.com")
	mustSucceed(t, r)
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash(t, s)), []byte("correct horse")); err != nil {
		t.Errorf("the stored hash does not match the new password: %v", err)
	}
	if got := s.count(t, "sessions"); got != 0 {
		t.Errorf("%d sessions survived a password change", got)
	}
	if !strings.Contains(r.out, "Signed out 2 active logins") {
		t.Errorf("output:\n%s", r.out)
	}
}

// TestPasswdPrompted also proves the prompts share one reader: a fresh reader per
// prompt would swallow the confirmation line into the first reader's buffer.
func TestPasswdPrompted(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "typed secret\ntyped secret\n", "passwd", "-email", "a@example.com")
	mustSucceed(t, r)
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash(t, s)), []byte("typed secret")); err != nil {
		t.Errorf("the prompted password was not stored: %v", err)
	}
}

func TestPasswdRefusals(t *testing.T) {
	s := newSite(t)
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"no account named", "", []string{"passwd"}, "give an account with -email"},
		{"unknown account", "", []string{"passwd", "-email", "x@example.com"}, "no account matches"},
		{"too short", "short\nshort\n", []string{"passwd", "-email", "a@example.com"}, "at least 8"},
		{"too long", strings.Repeat("x", 73) + "\n" + strings.Repeat("x", 73) + "\n", []string{"passwd", "-email", "a@example.com"}, "at most 72"},
		{"whitespace", "          \n          \n", []string{"passwd", "-email", "a@example.com"}, "only whitespace"},
		{"mismatch", "first one\nsecond one\n", []string{"passwd", "-email", "a@example.com"}, "do not match"},
		{"no input", "", []string{"passwd", "-email", "a@example.com"}, "read password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mustFail(t, yaba(t, s.env(), tt.stdin, tt.args...), 1, tt.want)
		})
	}
	// There is no -password flag: a password on the command line would sit
	// in the process list and the shell history.
	mustFail(t, yaba(t, s.env(), "", "passwd", "-email", "a@example.com", "-password", "longenough"),
		2, "flag provided but not defined")
	if passwordHash(t, s) != "hash" {
		t.Error("a refused change modified the password")
	}
}

func TestPasswdList(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "", "passwd", "-list")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "a@example.com") || !strings.Contains(r.out, "EMAIL") {
		t.Errorf("listing:\n%s", r.out)
	}
}

// ── repair ────────────────────────────────────────────────────────────────────

// addImpossibleDeposit books a fund deposit far beyond the household's cash.
func addImpossibleDeposit(t *testing.T, s site) int64 {
	t.Helper()
	sqlDB, err := db.Open(s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	res, err := sqlDB.Exec(`
		INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on, fund_id)
		VALUES(1, 1, 'fund_deposit', 'oops', 5000000000, '2026-01-03', (SELECT id FROM funds LIMIT 1))`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestRepairWithNothingToRepair(t *testing.T) {
	s := newSite(t)
	r := yaba(t, s.env(), "", "repair")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "No impossible fund movements found") {
		t.Errorf("output:\n%s", r.out)
	}
}

func TestRepairReportsAndDeletesOnConfirmation(t *testing.T) {
	s := newSite(t)
	bad := addImpossibleDeposit(t, s)
	id := strconv.FormatInt(bad, 10)

	// Report only.
	r := yaba(t, s.env(), "", "repair")
	mustSucceed(t, r)
	if !strings.Contains(r.out, "yaba repair -delete "+id) {
		t.Errorf("the report should give the command to run:\n%s", r.out)
	}
	if got := s.count(t, "transactions"); got != 3 {
		t.Fatalf("a report-only run changed the ledger: %d rows", got)
	}

	// An id it did not flag is refused outright.
	mustFail(t, yaba(t, s.env(), "delete\n", "repair", "-delete", "1"), 1, "was not reported as impossible")

	// Declined.
	mustFail(t, yaba(t, s.env(), "no\n", "repair", "-delete", id), 1, "cancelled")
	if got := s.count(t, "transactions"); got != 3 {
		t.Fatalf("a declined repair deleted rows: %d left", got)
	}

	// Confirmed: snapshot first, then the deletion, then a fresh diagnosis.
	r = yaba(t, s.env(), "delete\n", "repair", "-delete", id)
	mustSucceed(t, r)
	if got := s.count(t, "transactions"); got != 2 {
		t.Errorf("want the impossible row gone, %d rows left", got)
	}
	if found, _ := db.Snapshots(s.backups()); len(found) != 1 {
		t.Errorf("want one safety snapshot before the deletion, got %v", found)
	}
	if !strings.Contains(r.out, "No impossible movements remain") {
		t.Errorf("output:\n%s", r.out)
	}
}

// TestRestoreRoundTripThroughTheCLI: backup, damage, restore newest, and the data is
// back -- the whole drill an operator would run.
func TestRestoreRoundTripThroughTheCLI(t *testing.T) {
	s := newSite(t)
	s.snapshot(t)

	mustSucceed(t, yaba(t, s.env(), "", "reset", "-yes", "-backup=false"))
	if got := s.count(t, "transactions"); got != 0 {
		t.Fatalf("setup: reset left %d transactions", got)
	}

	r := yaba(t, s.env(), "", "restore", "-force")
	mustSucceed(t, r)
	if got := s.count(t, "transactions"); got != 2 {
		t.Errorf("after restore, %d transactions, want 2", got)
	}
	if _, err := db.VerifySnapshot(context.Background(), s.db); err != nil {
		t.Errorf("the restored database does not verify: %v", err)
	}
}
