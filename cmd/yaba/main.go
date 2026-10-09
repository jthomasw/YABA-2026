// Command yaba is the maintenance tool for a YABA database. It is kept out of the
// server binary so a destructive action is not one mistyped character away from the
// thing you run every day.
//
//	reset     empty the database, or reduce it to a single account
//	passwd    set an account's password, or list accounts
//	backup    take a verified snapshot, or list the ones you have
//	restore   put a snapshot back
//	repair    find fund movements that could not have happened, and remove them
//
// Run a subcommand with -h for its own flags. The defaults come from the same
// environment variables the server reads (YABA_DB, YABA_UPLOADS, YABA_BACKUP_DIR,
// YABA_BACKUP_KEEP), so running it with the server's environment file points it at the
// server's data.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/envfile"
	"github.com/jthomasw/YABA-2026/internal/money"
)

func main() {
	// The same .env the server reads, so the CLI finds the same database,
	// uploads and backups without being told.
	if _, err := envfile.Load(envfile.Path()); err != nil {
		fmt.Fprintf(os.Stderr, "yaba: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// app carries the command's input, output and environment, so the tests can drive
// every subcommand in-process: typed confirmations included, and without os.Exit.
type app struct {
	// One reader for the whole run. A fresh bufio.Reader per prompt would buffer past
	// the first line and silently swallow the answer to the next one.
	in *bufio.Reader
	// term is stdin when it is a file (a terminal, usually), so a password
	// prompt can switch echo off. Nil when the input is not a file, as in tests.
	term   *os.File
	out    io.Writer
	errOut io.Writer
	getenv func(string) string
}

// run dispatches on the subcommand and returns the process exit status: 0 success,
// 1 the command failed, 2 it was used wrongly. Each subcommand owns its own FlagSet, so
// two can define -db without colliding and -h prints only the flags that apply.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	a := &app{in: bufio.NewReader(stdin), out: stdout, errOut: stderr, getenv: getenv}
	if f, ok := stdin.(*os.File); ok {
		a.term = f
	}

	if len(args) < 1 {
		a.usage()
		return 2
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)

	// Flags shared by several subcommands, with the server's environment as defaults.
	dbFlag := func(usage string) *string {
		return fs.String("db", a.envOr("YABA_DB", "yaba.db"), usage+" (env YABA_DB)")
	}
	uploadsFlag := func(usage string) *string {
		return fs.String("uploads", a.envOr("YABA_UPLOADS", "uploads"), usage+" (env YABA_UPLOADS)")
	}
	dirFlag := func(usage string) *string {
		return fs.String("dir", a.envOr("YABA_BACKUP_DIR", ""),
			usage+` (env YABA_BACKUP_DIR; default "backups" beside -db, as the server does)`)
	}

	var exec func() error
	switch cmd {
	case "reset":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			uploadDir = uploadsFlag("receipt directory to clear")
			dir       = dirFlag("where the safety snapshot goes")
			keep      = fs.String("keep", "", "email of the one account to keep; everything else is deleted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
			backup    = fs.Bool("backup", true, "copy the database aside before wiping it")
		)
		exec = func() error {
			return a.runReset(*dbPath, *uploadDir, backupDir(*dir, *dbPath), *keep, *yes, *backup)
		}

	case "passwd":
		var (
			dbPath = dbFlag("path to the SQLite database")
			email  = fs.String("email", "", "email address of the account to update")
			list   = fs.Bool("list", false, "list accounts and exit")
		)
		// There is deliberately no -password flag: a flag value sits in the
		// process list and the shell history. The password is read from
		// stdin -- typed at a prompt that does not echo, or piped in as two
		// lines (the password and its confirmation).
		exec = func() error { return a.runPasswd(*dbPath, *email, *list) }

	case "backup":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			dir       = dirFlag("directory for snapshots")
			uploadDir = uploadsFlag("receipt directory to archive alongside")
			keep      = fs.Int("keep", int(a.envInt64("YABA_BACKUP_KEEP", db.DefaultBackupKeep)),
				"how many snapshots to retain (env YABA_BACKUP_KEEP)")
			list        = fs.Bool("list", false, "list existing snapshots and exit")
			acceptEmpty = fs.Bool("accept-empty", false,
				"accept a snapshot in which users, households, transactions or funds went from rows to none "+
					"(after a deliberate deletion of the last of them)")
		)
		exec = func() error {
			return a.runBackup(*dbPath, backupDir(*dir, *dbPath), *uploadDir, *keep, *list, *acceptEmpty)
		}

	case "restore":
		var (
			from   = fs.String("from", "", "snapshot to restore (omit to use the newest in -dir)")
			dir    = dirFlag("directory to look in when -from is omitted")
			dbPath = dbFlag("where to write the restored database; the only command that may create it")
			check  = fs.Bool("check", false, "verify the snapshot and stop without writing anything")
		)
		// Accepted so existing scripts keep working, and otherwise ignored: an existing
		// database is always moved aside, never overwritten, so there is nothing to force.
		fs.Bool("force", false, "accepted for compatibility; an existing database is always moved aside, never overwritten")
		exec = func() error { return a.runRestore(*from, backupDir(*dir, *dbPath), *dbPath, *check) }

	case "repair":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			dir       = dirFlag("where the safety snapshot goes")
			uploadDir = uploadsFlag("receipt directory to archive with the snapshot")
			del       = fs.String("delete", "", "comma-separated transaction ids to remove; only ids this command reported are accepted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
		)
		exec = func() error { return a.runRepair(*dbPath, backupDir(*dir, *dbPath), *uploadDir, *del, *yes) }

	case "help", "-h", "-help", "--help":
		a.usage()
		return 0

	default:
		fmt.Fprintf(stderr, "yaba: unknown subcommand %q\n\n", cmd)
		a.usage()
		return 2
	}

	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// "yaba backup list" would otherwise parse as no flags at all and take a backup.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "yaba %s: unexpected argument %q (flags start with -; see yaba %s -h)\n",
			cmd, fs.Arg(0), cmd)
		return 2
	}
	if err := exec(); err != nil {
		fmt.Fprintf(stderr, "yaba %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func (a *app) usage() {
	fmt.Fprint(a.errOut, `yaba - maintenance tool for a YABA database

Usage:
  yaba reset   [-db path] [-uploads dir] [-dir path] [-keep email] [-yes] [-backup=false]
  yaba passwd  [-db path] [-email addr] [-list]
  yaba backup  [-db path] [-dir path] [-uploads dir] [-keep n] [-list] [-accept-empty]
  yaba restore [-from snapshot] [-dir path] [-db path] [-check]
  yaba repair  [-db path] [-dir path] [-uploads dir] [-delete ids] [-yes]

Defaults come from YABA_DB, YABA_UPLOADS, YABA_BACKUP_DIR and YABA_BACKUP_KEEP, as
for the server. Every command except restore refuses a -db that does not exist.
reset, restore and repair also refuse to run while the server has the database open.

Run "yaba <subcommand> -h" for the flags of one subcommand.
`)
}

// backupDir applies the server's rule: an explicit directory wins, otherwise
// "backups" beside the database.
func backupDir(dir, dbPath string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return db.BackupDirFor(dbPath)
}

// requireDB refuses a database path that does not exist. db.Open would otherwise
// create an empty database there, and the command would go on to fail confusingly
// ("no such table") or report that there was nothing to do -- typically because it
// was run from the wrong directory with the relative default.
func requireDB(dbPath string) error {
	info, err := os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		where := ""
		if !filepath.IsAbs(dbPath) {
			if abs, err := filepath.Abs(dbPath); err == nil {
				where = " (" + abs + ", relative to the current directory)"
			}
		}
		return fmt.Errorf("no database at %s%s; nothing was created. "+
			"Pass -db or set YABA_DB to the server's database "+
			"(in production, YABA_DB in /etc/yaba.env)", dbPath, where)
	}
	if err != nil {
		return fmt.Errorf("check %s: %w", dbPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a database file", dbPath)
	}
	return nil
}

// lockDB takes the database's lock for a command that must not run beside the server.
func lockDB(dbPath string) (*db.Lock, error) {
	lock, err := db.AcquireLock(dbPath)
	if errors.Is(err, db.ErrLocked) {
		return nil, fmt.Errorf("%w. Stop the server first (sudo systemctl stop yaba), "+
			"run this again, then start it", err)
	}
	return lock, err
}

// confirm asks for a typed word, so a destructive step is never one Enter away.
func (a *app) confirm(question, word string) bool {
	if question != "" {
		fmt.Fprint(a.out, question, " ")
	}
	fmt.Fprintf(a.out, "Type '%s' to continue: ", word)
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line) == word
}

func (a *app) runReset(dbPath, uploadDir, backupDir, keep string, yes, backup bool) error {
	// A missing database used to print "nothing to reset" and succeed, which is
	// exactly what running from the wrong directory looks like.
	if err := requireDB(dbPath); err != nil {
		return err
	}
	lock, err := lockDB(dbPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// Show what is about to go, so a confirmation prompt is an informed one
	// rather than a reflex.
	if err := a.summarise(sqlDB); err != nil {
		// A database too old or too broken to summarise can still be reset.
		fmt.Fprintf(a.out, "(could not summarise existing data: %v)\n", err)
	}

	keep = strings.ToLower(strings.TrimSpace(keep))

	// In keep mode, confirm the account exists before asking anything: a typo in
	// the address would otherwise delete every user including the intended one.
	var keepID int64
	if keep != "" {
		err := sqlDB.QueryRow(`
			SELECT id FROM users
			WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE
			LIMIT 1`, keep, keep).Scan(&keepID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no account matches %q — nothing was changed", keep)
		}
		if err != nil {
			return fmt.Errorf("look up the account to keep: %w", err)
		}
		fmt.Fprintf(a.out, "\nKeeping account %d (%s) and everything belonging to it.\n", keepID, keep)
		fmt.Fprintln(a.out, "Every other account and all of its data will be deleted.")
	}

	if !yes {
		word := "reset"
		if keep != "" {
			word = "prune"
		}
		if !a.confirm("\nThis permanently deletes the data described above.", word) {
			return errors.New("cancelled")
		}
	}

	if backup {
		// A verified snapshot taken on the open connection.
		snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
			Dir:       backupDir,
			UploadDir: uploadDir,
		})
		if err != nil {
			// Refuse to continue. The backup exists precisely because the next
			// step is irreversible.
			return fmt.Errorf("backup failed, so nothing was deleted: %w", err)
		}
		fmt.Fprintf(a.out, "Backed up to %s\n", snap.Path)
		if snap.Uploads != "" {
			fmt.Fprintf(a.out, "Receipts archived to %s\n", snap.Uploads)
		}
	}

	if keep != "" {
		if err := a.pruneToOneUser(sqlDB, keepID); err != nil {
			return err
		}
		if uploadDir != "" {
			if err := a.clearUploadsExcept(sqlDB, uploadDir, keepID); err != nil {
				fmt.Fprintf(a.out, "(could not tidy %s: %v)\n", uploadDir, err)
			}
		}
		fmt.Fprintln(a.out, "\nDone. Only that account and its data remain.")
		return a.summarise(sqlDB)
	}

	if err := a.dropEverything(sqlDB); err != nil {
		return err
	}

	// Rebuild from migration 1, so the reset database is immediately usable
	// rather than empty until the next server start.
	if err := db.Migrate(sqlDB); err != nil {
		return fmt.Errorf("rebuild schema: %w", err)
	}

	if uploadDir != "" {
		if err := clearDir(uploadDir); err != nil {
			fmt.Fprintf(a.out, "(could not clear %s: %v)\n", uploadDir, err)
		} else {
			fmt.Fprintf(a.out, "Cleared %s\n", uploadDir)
		}
	}

	fmt.Fprintln(a.out, "\nDone. The database is empty and the schema is current.")
	fmt.Fprintln(a.out, "Start the server and sign up to create the first account.")
	return nil
}

// pruneToOneUser deletes every account except keepID and everything belonging to it.
func (a *app) pruneToOneUser(sqlDB *sql.DB, keepID int64) error {
	var fkOn int
	if err := sqlDB.QueryRow(`PRAGMA foreign_keys`).Scan(&fkOn); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	if fkOn != 1 {
		return errors.New("foreign keys are off, so deleting a user would orphan their rows instead of removing them")
	}

	// Shared households first. A row in a shared household is owned by the household but
	// its user_id still cascades, so deleting another account would remove entries the
	// kept user still sees. Reassigning attribution keeps the rows, and the totals.
	for _, table := range []string{
		"transactions", "funds", "expense_buckets", "allocations", "budgets", "receipt_jobs",
		"recurring_income", "recurring_expense",
	} {
		res, err := sqlDB.Exec(`
			UPDATE `+table+` SET user_id = ?
			WHERE user_id <> ?
			  AND household_id IN (SELECT household_id FROM household_members WHERE user_id = ?)`,
			keepID, keepID, keepID)
		if err != nil {
			return fmt.Errorf("reassign %s in shared households: %w", table, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Fprintf(a.out, "Kept %d %s row(s) from a shared budget, now credited to you.\n", n, table)
		}
	}

	res, err := sqlDB.Exec(`DELETE FROM users WHERE id <> ?`, keepID)
	if err != nil {
		return fmt.Errorf("delete other accounts: %w", err)
	}
	n, _ := res.RowsAffected()
	fmt.Fprintf(a.out, "Deleted %d account(s) and everything belonging to them.\n", n)

	// A kept editor may be the only person left in a shared household.  Promote
	// that surviving member so the budget remains usable and the database keeps
	// its invariant that every household has an owner.
	if _, err := sqlDB.Exec(`
		UPDATE household_members AS kept
		SET role = 'owner'
		WHERE kept.user_id = ?
		  AND NOT EXISTS (SELECT 1 FROM household_members owner
		                  WHERE owner.household_id = kept.household_id AND owner.role = 'owner')`, keepID); err != nil {
		return fmt.Errorf("promote sole remaining shared member: %w", err)
	}

	// A shared household whose members have all been deleted is unreachable but still on
	// disk. Personal ones cascade away with their owner, so only shared ones need this.
	res, err = sqlDB.Exec(`
		DELETE FROM households
		WHERE personal_for IS NULL
		  AND NOT EXISTS (SELECT 1 FROM household_members m WHERE m.household_id = households.id)`)
	if err != nil {
		return fmt.Errorf("delete memberless households: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(a.out, "Removed %d shared budget(s) that no longer had any members.\n", n)
	}

	// A pending invitation to a deleted account would show a banner to whoever signs up
	// with that address next.
	if _, err := sqlDB.Exec(`
		UPDATE household_invites SET status = 'revoked', responded_at = datetime('now')
		WHERE status = 'pending'
		  AND email NOT IN (SELECT IFNULL(email, username) FROM users)`); err != nil {
		return fmt.Errorf("revoke stale invitations: %w", err)
	}

	// login_attempts is keyed on ip|email, so no foreign key reaches it.
	if _, err := sqlDB.Exec(`DELETE FROM login_attempts`); err != nil {
		fmt.Fprintf(a.out, "(could not clear login attempts: %v)\n", err)
	}

	// The legacy_* archives hold the very rows being removed and no cascade reaches them,
	// so a cleared database would still hold old transactions and password hashes.
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for the legacy drop: %w", err)
	}

	rows, err := sqlDB.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name LIKE 'legacy_%'`)
	if err != nil {
		return fmt.Errorf("list legacy tables: %w", err)
	}
	var legacy []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	dropped := make([]string, 0, len(legacy))
	for _, t := range legacy {
		if _, err := sqlDB.Exec(`DROP TABLE IF EXISTS "` + t + `"`); err != nil {
			// Report and carry on: the user's own data has already been pruned
			// correctly, and a leftover archive table is untidy rather than wrong.
			fmt.Fprintf(a.out, "(could not drop %s: %v)\n", t, err)
			continue
		}
		dropped = append(dropped, t)
	}
	legacy = dropped
	if len(legacy) > 0 {
		fmt.Fprintf(a.out, "Dropped %d legacy archive table(s): %s\n", len(legacy), strings.Join(legacy, ", "))
	}

	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("re-enable foreign keys: %w", err)
	}

	// Confirm nothing was orphaned rather than trusting the cascade.
	if bad, err := sqlDB.Query(`PRAGMA foreign_key_check`); err == nil {
		defer bad.Close()
		if bad.Next() {
			return errors.New("foreign_key_check found orphaned rows after the delete")
		}
		fmt.Fprintln(a.out, "Integrity check: clean.")
	}

	if _, err := sqlDB.Exec(`VACUUM`); err != nil {
		fmt.Fprintf(a.out, "(vacuum skipped: %v)\n", err)
	}
	return nil
}

// clearUploadsExcept removes every user's receipt directory but the one kept: files
// under uploads/<user id>/ are on disk, where no database cascade reaches them.
func (a *app) clearUploadsExcept(sqlDB *sql.DB, dir string, keepID int64) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	// A shared entry is re-attributed to the kept user above, but its receipt is
	// still physically stored in the uploader's directory.  Preserve every
	// directory referenced by a surviving row instead of assuming keepID owns it.
	keep := map[string]bool{strconv.FormatInt(keepID, 10): true}
	rows, err := sqlDB.Query(`SELECT receipt_path FROM transactions WHERE receipt_path IS NOT NULL AND receipt_path <> ''`)
	if err != nil {
		return fmt.Errorf("list surviving receipt paths: %w", err)
	}
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			rows.Close()
			return err
		}
		rel, err := filepath.Rel(dir, filepath.FromSlash(stored))
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
			continue
		}
		first := strings.Split(filepath.Clean(rel), string(os.PathSeparator))[0]
		if first != "" && first != "." {
			keep[first] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	removed := 0
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 {
		fmt.Fprintf(a.out, "Removed %d other user's receipt folder(s).\n", removed)
	}
	return nil
}

// summarise prints a row count per table.
func (a *app) summarise(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(tables) == 0 {
		fmt.Fprintln(a.out, "The database has no tables.")
		return nil
	}

	fmt.Fprintln(a.out, "Current contents:")
	for _, t := range tables {
		var n int
		// The table name cannot be a bound parameter, and it came from
		// sqlite_master rather than from user input, so quoting it is enough.
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM "` + t + `"`).Scan(&n); err != nil {
			fmt.Fprintf(a.out, "  %-28s (unreadable)\n", t)
			continue
		}
		fmt.Fprintf(a.out, "  %-28s %d rows\n", t, n)
	}
	return nil
}

// dropEverything removes every table and view. Dropping rather than deleting the file,
// which may be held open by a syncing client and would lose its permissions.
func (a *app) dropEverything(sqlDB *sql.DB) error {
	// Foreign keys off for the duration, or dropping a parent table cascades and the drop
	// order starts to matter.
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer sqlDB.Exec(`PRAGMA foreign_keys = ON`)

	for _, kind := range []string{"view", "table"} {
		rows, err := sqlDB.Query(`
			SELECT name FROM sqlite_master
			WHERE type = ? AND name NOT LIKE 'sqlite_%'`, kind)
		if err != nil {
			return fmt.Errorf("list %ss: %w", kind, err)
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, n := range names {
			stmt := fmt.Sprintf(`DROP %s IF EXISTS "%s"`, strings.ToUpper(kind), n)
			if _, err := sqlDB.Exec(stmt); err != nil {
				return fmt.Errorf("drop %s %s: %w", kind, n, err)
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(a.out, "Dropped %d %s(s).\n", len(names), kind)
		}
	}

	// Reset the AUTOINCREMENT high-water marks so new ids start at 1.
	sqlDB.Exec(`DELETE FROM sqlite_sequence`)

	// Reclaim the space the dropped data occupied.
	if _, err := sqlDB.Exec(`VACUUM`); err != nil {
		fmt.Fprintf(a.out, "(vacuum skipped: %v)\n", err)
	}
	return nil
}

// ── repair ────────────────────────────────────────────────────────────────────

// anomaly is one fund movement that could not have happened when it did.
type anomaly struct {
	TxID        int64
	Household   int64
	Kind        string
	Label       string
	OccurredOn  string
	Amount      money.Cents
	Available   money.Cents // cash, or the fund's balance, immediately before it
	AvailableOf string      // what Available is a measure of
}

// findAnomalies replays each household's ledger in order and reports movements that
// were impossible when recorded: a deposit above available cash, or a withdrawal above
// the fund's balance.
func findAnomalies(sqlDB *sql.DB) ([]anomaly, error) {
	rows, err := sqlDB.Query(`
		SELECT household_id, id, kind, label, amount_cents, occurred_on, IFNULL(fund_id, 0)
		FROM transactions
		ORDER BY household_id, occurred_on,
			CASE kind
				WHEN 'income' THEN 0
				WHEN 'fund_withdrawal' THEN 1
				WHEN 'expense' THEN 2
				WHEN 'fund_deposit' THEN 3
				ELSE 4
			END,
			id`)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()

	cash := map[int64]money.Cents{}    // per household
	balance := map[int64]money.Cents{} // per fund
	var found []anomaly

	for rows.Next() {
		var hh, id, fundID, cents int64
		var kind, label, on string
		if err := rows.Scan(&hh, &id, &kind, &label, &cents, &on, &fundID); err != nil {
			return nil, fmt.Errorf("scan ledger: %w", err)
		}
		// Scanned as int64 and converted, matching the store's convention rather
		// than relying on the driver to fill a named integer type.
		amount := money.Cents(cents)

		switch kind {
		case "income":
			cash[hh] += amount
		case "expense":
			cash[hh] -= amount
		case "fund_deposit":
			if amount > cash[hh] {
				found = append(found, anomaly{id, hh, kind, label, on, amount, cash[hh], "cash"})
			}
			cash[hh] -= amount
			balance[fundID] += amount
		case "fund_withdrawal":
			if amount > balance[fundID] {
				found = append(found, anomaly{id, hh, kind, label, on, amount, balance[fundID], "in that fund"})
			}
			cash[hh] += amount
			balance[fundID] -= amount
		}
	}
	return found, rows.Err()
}

// householdCash is the cash a household holds, optionally ignoring some rows.
func householdCash(sqlDB *sql.DB, household int64, ignoring []int64) (money.Cents, error) {
	q := `SELECT IFNULL(SUM(CASE WHEN kind IN ('income','fund_withdrawal')
	                             THEN amount_cents ELSE -amount_cents END), 0)
	      FROM transactions WHERE household_id = ?`
	args := []any{household}
	for _, id := range ignoring {
		q += ` AND id <> ?`
		args = append(args, id)
	}
	var cents int64
	err := sqlDB.QueryRow(q, args...).Scan(&cents)
	return money.Cents(cents), err
}

func (a *app) runRepair(dbPath, backupDir, uploadDir, del string, yes bool) error {
	if err := requireDB(dbPath); err != nil {
		return err
	}
	// The diagnosis is a replay of the whole ledger. A server writing while it runs
	// would make the reported ids and the "cash X → Y" lines describe a ledger that no
	// longer exists by the time the deletion is confirmed.
	lock, err := lockDB(dbPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	found, err := findAnomalies(sqlDB)
	if err != nil {
		return err
	}

	if len(found) == 0 {
		fmt.Fprintln(a.out, "No impossible fund movements found.")
		return nil
	}

	fmt.Fprintf(a.out, "%d fund movement(s) that could not have happened when they did:\n\n", len(found))
	byHousehold := map[int64][]int64{}
	for _, an := range found {
		fmt.Fprintf(a.out, "  transaction %d  household %d  %s\n", an.TxID, an.Household, an.OccurredOn)
		fmt.Fprintf(a.out, "      %s of %s labelled %q\n", an.Kind, an.Amount.Display(), an.Label)
		fmt.Fprintf(a.out, "      but only %s was %s at that point\n\n",
			an.Available.Display(), an.AvailableOf)
		byHousehold[an.Household] = append(byHousehold[an.Household], an.TxID)
	}

	// What removing them would do, so the decision is an informed one.
	fmt.Fprintln(a.out, "Effect of removing them:")
	for hh, ids := range byHousehold {
		before, err := householdCash(sqlDB, hh, nil)
		if err != nil {
			return err
		}
		after, err := householdCash(sqlDB, hh, ids)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "  household %d: cash %s → %s\n", hh, before.Display(), after.Display())
	}

	if del == "" {
		fmt.Fprintln(a.out, "\nNothing was changed. To remove specific rows:")
		fmt.Fprintf(a.out, "  yaba repair -delete %s\n", joinIDs(allIDs(found)))
		fmt.Fprintln(a.out, "\nRemoving only some of them may leave the household still")
		fmt.Fprintln(a.out, "inconsistent, which is why the ids are yours to choose.")
		return nil
	}

	// Only ids this command reported are accepted. A typo would otherwise delete
	// an ordinary transaction, and there is no undo beyond the snapshot.
	wanted, err := parseIDs(del)
	if err != nil {
		return err
	}
	flagged := map[int64]anomaly{}
	for _, an := range found {
		flagged[an.TxID] = an
	}
	for _, id := range wanted {
		if _, ok := flagged[id]; !ok {
			return fmt.Errorf("transaction %d was not reported as impossible; "+
				"this command will only delete rows it flagged", id)
		}
	}

	fmt.Fprintf(a.out, "\nAbout to permanently delete %d transaction(s): %s\n",
		len(wanted), joinIDs(wanted))
	if !yes && !a.confirm("", "delete") {
		return errors.New("cancelled")
	}

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
		Dir: backupDir, UploadDir: uploadDir,
	})
	if err != nil {
		return fmt.Errorf("backup failed, so nothing was deleted: %w", err)
	}
	fmt.Fprintf(a.out, "Backed up to %s\n", snap.Path)

	// One transaction: either every named row goes or none does.
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	for _, id := range wanted {
		if _, err := tx.Exec(`DELETE FROM transactions WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("delete transaction %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deletions: %w", err)
	}
	fmt.Fprintf(a.out, "Deleted %d transaction(s).\n\n", len(wanted))

	// Re-run the diagnosis so the result is measured, not assumed.
	left, err := findAnomalies(sqlDB)
	if err != nil {
		return err
	}
	for hh := range byHousehold {
		c, err := householdCash(sqlDB, hh, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "  household %d now holds %s in cash\n", hh, c.Display())
	}
	if len(left) == 0 {
		fmt.Fprintln(a.out, "\nNo impossible movements remain.")
	} else {
		fmt.Fprintf(a.out, "\n%d impossible movement(s) still present: %s\n",
			len(left), joinIDs(allIDs(left)))
		fmt.Fprintln(a.out, "Run this command again to see them.")
	}
	return nil
}

func allIDs(found []anomaly) []int64 {
	ids := make([]int64, 0, len(found))
	for _, a := range found {
		ids = append(ids, a.TxID)
	}
	return ids
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func parseIDs(s string) ([]int64, error) {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a transaction id", part)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("no transaction ids given")
	}
	return out, nil
}

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

// envInt64 reads a positive integer from the environment.
func (a *app) envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(a.getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		fmt.Fprintf(a.errOut, "warning: %s=%q is not a positive integer; using %d\n",
			key, v, fallback)
		return fallback
	}
	return n
}

// envOr matches the server's envOr exactly (trimmed, empty means unset), so both read
// one environment file the same way.
func (a *app) envOr(key, fallback string) string {
	if v := strings.TrimSpace(a.getenv(key)); v != "" {
		return v
	}
	return fallback
}

// ── passwd ────────────────────────────────────────────────────────────────────

func (a *app) runPasswd(dbPath, email string, list bool) error {
	// No lock: the change is one SQLite transaction, safe beside the running server,
	// and resetting a password is often needed precisely while it is serving.
	if err := requireDB(dbPath); err != nil {
		return err
	}
	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// Deliberately does not run migrations: upgrading a schema as a side effect of a
	// password reset would be surprising and hard to undo.

	if list {
		return a.listAccounts(sqlDB)
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("give an account with -email, or use -list to see them")
	}

	var id int64
	var current string
	err = sqlDB.QueryRow(`
		SELECT id, IFNULL(email, username) FROM users
		WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE
		LIMIT 1`, email, email).Scan(&id, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account matches %q (try -list)", email)
	}
	if err != nil {
		return fmt.Errorf("look up account: %w", err)
	}

	fmt.Fprintf(a.out, "Setting a new password for %s (id %d).\n", current, id)
	password, err := a.promptSecret("New password: ")
	if err != nil {
		return err
	}
	again, err := a.promptSecret("Confirm password: ")
	if err != nil {
		return err
	}
	if password != again {
		return errors.New("the two passwords do not match")
	}

	if err := checkPassword(password); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// The password and the session revocation share one transaction.
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), id)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("expected to update 1 row, updated %d", n)
	}

	// Every device holding a session is signed out, which is what makes a password change
	// a real response to a suspected compromise.
	revoked, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	n, _ := revoked.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM password_resets WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("revoke password reset links: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Fprintf(a.out, "Password updated for %s.\n", current)
	switch n {
	case 0:
		fmt.Fprintln(a.out, "No active logins to sign out.")
	case 1:
		fmt.Fprintln(a.out, "Signed out 1 active login. It will be asked to log in again.")
	default:
		fmt.Fprintf(a.out, "Signed out %d active logins. They will be asked to log in again.\n", n)
	}
	return nil
}

func (a *app) listAccounts(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), created_at
		FROM users ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	fmt.Fprintf(a.out, "%-5s %-34s %-16s %s\n", "ID", "EMAIL", "NAME", "CREATED")
	for rows.Next() {
		var id int64
		var email, name, created string
		if err := rows.Scan(&id, &email, &name, &created); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%-5d %-34s %-16s %s\n", id, email, name, created)
	}
	return rows.Err()
}

// checkPassword applies the same rules the signup form does, so a password set
// here cannot be one the application would have refused.
func checkPassword(p string) error {
	if len(p) < 8 {
		return errors.New("passwords need at least 8 characters")
	}
	// bcrypt truncates silently at 72 bytes, so a longer password would be
	// accepted here and then only partly checked at login.
	if len(p) > 72 {
		return errors.New("passwords can be at most 72 characters")
	}
	if strings.TrimSpace(p) == "" {
		return errors.New("that password is only whitespace")
	}
	return nil
}

// promptSecret reads a password from stdin. At a terminal, echo is switched off
// while it is typed, so it is not left on the screen; piped input is read as is.
func (a *app) promptSecret(label string) (string, error) {
	fmt.Fprint(a.out, label)
	restore := echoOff(a.term)
	line, err := a.in.ReadString('\n')
	if restore != nil {
		restore()
		fmt.Fprintln(a.out) // the Enter that was typed was not echoed either
	}
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
