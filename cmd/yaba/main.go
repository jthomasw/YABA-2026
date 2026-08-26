// Command yaba is the maintenance tool for a YABA database.
//
// Kept out of the server binary because a destructive action should not be one
// mistyped character away from the thing you run every day:
//
//	reset     empty the database, or reduce it to a single account
//	passwd    set an account's password, or list accounts
//	backup    take a verified snapshot, or list the ones you have
//	restore   put a snapshot back
//	repair    find fund movements that could not have happened, and remove them
//
// Usage:
//
//	go run ./cmd/yaba reset                                # wipe everything
//	go run ./cmd/yaba reset -keep you@example.com          # keep one account
//	go run ./cmd/yaba passwd -list
//	go run ./cmd/yaba passwd -email someone@example.com
//	go run ./cmd/yaba backup                               # snapshot now
//	go run ./cmd/yaba backup -list
//	go run ./cmd/yaba restore -check                       # prove the newest one works
//	go run ./cmd/yaba restore -db restored.db              # the quarterly drill
//
// Run a subcommand with -h for its own flags.
package main


import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/money"
)

// main dispatches on the subcommand.
//
// Each subcommand owns a FlagSet rather than sharing the global one, so `reset`
// and `passwd` can both define -db without colliding, and -h prints only the
// flags that apply to the subcommand actually being run.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "reset":
		fs := flag.NewFlagSet("reset", flag.ExitOnError)
		var (
			dbPath    = fs.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
			uploadDir = fs.String("uploads", envOr("YABA_UPLOADS", "uploads"), "receipt directory to clear")
			keep      = fs.String("keep", "", "email of the one account to keep; everything else is deleted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
			backup    = fs.Bool("backup", true, "copy the database aside before wiping it")
		)
		fs.Parse(os.Args[2:])
		if err := runReset(*dbPath, *uploadDir, *keep, *yes, *backup); err != nil {
			fmt.Fprintf(os.Stderr, "yaba reset: %v\n", err)
			os.Exit(1)
		}

	case "passwd":
		fs := flag.NewFlagSet("passwd", flag.ExitOnError)
		var (
			dbPath   = fs.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
			email    = fs.String("email", "", "email address of the account to update")
			password = fs.String("password", "", "new password (omit to be prompted)")
			list     = fs.Bool("list", false, "list accounts and exit")
		)
		fs.Parse(os.Args[2:])
		if err := runPasswd(*dbPath, *email, *password, *list); err != nil {
			fmt.Fprintf(os.Stderr, "yaba passwd: %v\n", err)
			os.Exit(1)
		}

	case "backup":
		fs := flag.NewFlagSet("backup", flag.ExitOnError)
		var (
			dbPath    = fs.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
			dir       = fs.String("dir", envOr("YABA_BACKUP_DIR", db.DefaultBackupDir()), "directory for snapshots")
			uploadDir = fs.String("uploads", envOr("YABA_UPLOADS", "uploads"), "receipt directory to archive alongside")
			keep      = fs.Int("keep", int(envInt64("YABA_BACKUP_KEEP", db.DefaultBackupKeep)), "how many snapshots to retain")
			list      = fs.Bool("list", false, "list existing snapshots and exit")
		)
		fs.Parse(os.Args[2:])
		if err := runBackup(*dbPath, *dir, *uploadDir, *keep, *list); err != nil {
			fmt.Fprintf(os.Stderr, "yaba backup: %v\n", err)
			os.Exit(1)
		}

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ExitOnError)
		var (
			from   = fs.String("from", "", "snapshot to restore (omit to use the newest in -dir)")
			dir    = fs.String("dir", envOr("YABA_BACKUP_DIR", db.DefaultBackupDir()), "directory to look in when -from is omitted")
			dbPath = fs.String("db", envOr("YABA_DB", "yaba.db"), "where to write the restored database")
			force  = fs.Bool("force", false, "overwrite an existing database")
			check  = fs.Bool("check", false, "verify the snapshot and stop without writing anything")
		)
		fs.Parse(os.Args[2:])
		if err := runRestore(*from, *dir, *dbPath, *force, *check); err != nil {
			fmt.Fprintf(os.Stderr, "yaba restore: %v\n", err)
			os.Exit(1)
		}

	case "repair":
		fs := flag.NewFlagSet("repair", flag.ExitOnError)
		var (
			dbPath    = fs.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
			dir       = fs.String("dir", envOr("YABA_BACKUP_DIR", db.DefaultBackupDir()), "where the safety snapshot goes")
			uploadDir = fs.String("uploads", envOr("YABA_UPLOADS", "uploads"), "receipt directory to archive with the snapshot")
			del       = fs.String("delete", "", "comma-separated transaction ids to remove; only ids this command reported are accepted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
		)
		fs.Parse(os.Args[2:])
		if err := runRepair(*dbPath, *dir, *uploadDir, *del, *yes); err != nil {
			fmt.Fprintf(os.Stderr, "yaba repair: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "yaba: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `yaba - maintenance tool for a YABA database

Usage:
  yaba reset   [-db path] [-uploads dir] [-keep email] [-yes] [-backup=false]
  yaba passwd  [-db path] [-email addr] [-password pw] [-list]
  yaba backup  [-db path] [-dir path] [-uploads dir] [-keep n] [-list]
  yaba restore [-from snapshot] [-dir path] [-db path] [-force] [-check]
  yaba repair  [-db path] [-delete ids] [-yes]

Run "yaba <subcommand> -h" for the flags of one subcommand.
`)
}

func runReset(dbPath, uploadDir, keep string, yes, backup bool) error {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("%s does not exist — nothing to reset. The server will create it.\n", dbPath)
		return nil
	}

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}

	// Show what is about to go, so a confirmation prompt is an informed one
	// rather than a reflex.
	if err := summarise(sqlDB); err != nil {
		// A database too old or too broken to summarise can still be reset.
		fmt.Printf("(could not summarise existing data: %v)\n", err)
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
			sqlDB.Close()
			return fmt.Errorf("no account matches %q — nothing was changed", keep)
		}
		if err != nil {
			sqlDB.Close()
			return fmt.Errorf("look up the account to keep: %w", err)
		}
		fmt.Printf("\nKeeping account %d (%s) and everything belonging to it.\n", keepID, keep)
		fmt.Println("Every other account and all of its data will be deleted.")
	}

	if !yes {
		word := "reset"
		if keep != "" {
			word = "prune"
		}
		fmt.Printf("\nThis permanently deletes the data described above. Type '%s' to continue: ", word)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != word {
			sqlDB.Close()
			return errors.New("cancelled")
		}
	}

	if backup {
		// A verified snapshot, taken on the open connection.
		//
		// This used to close the database and copy the file, on the reasoning
		// that WAL keeps recent writes in a sidecar so an open file is unsafe to
		// copy. The reasoning was right and the remedy was wrong: closing takes
		// the database offline, the copy landed next to the original where a
		// single mistaken delete removes both, and nothing ever checked that the
		// copy was readable. VACUUM INTO needs no downtime, writes somewhere
		// else, and the snapshot is verified before this command deletes
		// anything.
		snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
			Dir:       envOr("YABA_BACKUP_DIR", db.DefaultBackupDir()),
			UploadDir: uploadDir,
		})
		if err != nil {
			// Refuse to continue. The backup exists precisely because the next
			// step is irreversible.
			return fmt.Errorf("backup failed, so nothing was deleted: %w", err)
		}
		fmt.Printf("Backed up to %s\n", snap.Path)
		if snap.Uploads != "" {
			fmt.Printf("Receipts archived to %s\n", snap.Uploads)
		}
	}
	defer sqlDB.Close()

	if keep != "" {
		if err := pruneToOneUser(sqlDB, keepID); err != nil {
			return err
		}
		if uploadDir != "" {
			if err := clearUploadsExcept(uploadDir, keepID); err != nil {
				fmt.Printf("(could not tidy %s: %v)\n", uploadDir, err)
			}
		}
		fmt.Println("\nDone. Only that account and its data remain.")
		return summarise(sqlDB)
	}

	if err := dropEverything(sqlDB); err != nil {
		return err
	}

	// Rebuild from migration 1, so the reset database is immediately usable
	// rather than empty until the next server start.
	if err := db.Migrate(sqlDB); err != nil {
		return fmt.Errorf("rebuild schema: %w", err)
	}

	if uploadDir != "" {
		if err := clearDir(uploadDir); err != nil {
			fmt.Printf("(could not clear %s: %v)\n", uploadDir, err)
		} else {
			fmt.Printf("Cleared %s\n", uploadDir)
		}
	}

	fmt.Println("\nDone. The database is empty and the schema is current.")
	fmt.Println("Start the server and sign up to create the first account.")
	return nil
}

// pruneToOneUser deletes every account except keepID, and everything belonging
// to them.
//
// One DELETE does almost all of it. Every per-user table declares
// `user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE`, and the
// tables below those cascade in turn -- line_items through transactions,
// allocations through both transactions and expense_buckets. So removing a user
// row removes their transactions, funds, buckets, allocations, line items,
// budgets, receipt jobs and notifications, with no chance of missing one.
//
// That only holds with foreign keys switched ON, which internal/db.Open does via
// the DSN. It is asserted here rather than assumed, because silently leaving
// orphaned rows behind is exactly the failure this design is meant to prevent.
func pruneToOneUser(sqlDB *sql.DB, keepID int64) error {
	var fkOn int
	if err := sqlDB.QueryRow(`PRAGMA foreign_keys`).Scan(&fkOn); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	if fkOn != 1 {
		return errors.New("foreign keys are off, so deleting a user would orphan their rows instead of removing them")
	}

	// ── shared households come first ────────────────────────────────────────
	//
	// Migration 4 introduced a case the plain cascade above gets wrong.
	//
	// A row in a SHARED household is owned by the household, but its user_id
	// still points at whoever entered it -- and that column is ON DELETE CASCADE,
	// inherited from a schema written when user_id meant ownership. So deleting
	// another account would take away the entries that person contributed to a
	// household the kept user is still in, silently changing that household's
	// totals.
	//
	// Fixing the column would mean rebuilding transactions, which this schema
	// forbids for good reason (see internal/db/migrate004.go). Reassigning the
	// attribution first achieves the same end: the rows survive, credited to the
	// account that remains.
	//
	// This is the only place an account is ever deleted -- the application itself
	// has no account-deletion path -- so handling it here covers it entirely.
	for _, table := range []string{
		"transactions", "funds", "expense_buckets", "allocations", "budgets", "receipt_jobs",
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
			fmt.Printf("Kept %d %s row(s) from a shared budget, now credited to you.\n", n, table)
		}
	}

	res, err := sqlDB.Exec(`DELETE FROM users WHERE id <> ?`, keepID)
	if err != nil {
		return fmt.Errorf("delete other accounts: %w", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("Deleted %d account(s) and everything belonging to them.\n", n)

	// A shared household whose every member has just been deleted is unreachable:
	// nothing can list it and nobody can switch into it, but its transactions are
	// still on disk. Personal households cascade away with their owner, so only
	// the shared ones need this.
	res, err = sqlDB.Exec(`
		DELETE FROM households
		WHERE personal_for IS NULL
		  AND NOT EXISTS (SELECT 1 FROM household_members m WHERE m.household_id = households.id)`)
	if err != nil {
		return fmt.Errorf("delete memberless households: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("Removed %d shared budget(s) that no longer had any members.\n", n)
	}

	// Invitations addressed to the deleted accounts are meaningless now, and one
	// left pending would show a banner to anybody who later signed up with that
	// address.
	if _, err := sqlDB.Exec(`
		UPDATE household_invites SET status = 'revoked', responded_at = datetime('now')
		WHERE status = 'pending'
		  AND email NOT IN (SELECT IFNULL(email, username) FROM users)`); err != nil {
		return fmt.Errorf("revoke stale invitations: %w", err)
	}

	// login_attempts is keyed on "ip|email", not on a user id, so no foreign key
	// reaches it and the rows for deleted accounts would survive. Harmless, but a
	// lockout recorded against an address that no longer exists would block the
	// person who signs up with it next.
	if _, err := sqlDB.Exec(`DELETE FROM login_attempts`); err != nil {
		fmt.Printf("(could not clear login attempts: %v)\n", err)
	}

	// The legacy_* tables are archives of the old pre-migration schema. They hold
	// the very rows being removed, and no cascade reaches them, so they have to be
	// dropped explicitly -- otherwise the "cleared" database would still contain
	// every old user's transactions and password hashes.
	//
	// Foreign keys have to be OFF for this, and the reason is subtle: migration 1
	// renamed funds -> legacy_funds, and SQLite rewrites REFERENCES clauses when a
	// table is renamed. So legacy_fund_transactions now points at legacy_funds.
	// With keys enforced, DROP TABLE performs an implicit DELETE of the parent's
	// rows, which the child's still-live constraint refuses -- the drop fails with
	// "FOREIGN KEY constraint failed".
	//
	// Switching them off is safe here in a way it would NOT be for the DELETE
	// above: dropping a whole table cannot orphan a row in a table that is also
	// being dropped, whereas deleting a user with keys off would leave their
	// transactions behind. Hence the narrow window, re-enabled immediately after.
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
			fmt.Printf("(could not drop %s: %v)\n", t, err)
			continue
		}
		dropped = append(dropped, t)
	}
	legacy = dropped
	if len(legacy) > 0 {
		fmt.Printf("Dropped %d legacy archive table(s): %s\n", len(legacy), strings.Join(legacy, ", "))
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
		fmt.Println("Integrity check: clean.")
	}

	if _, err := sqlDB.Exec(`VACUUM`); err != nil {
		fmt.Printf("(vacuum skipped: %v)\n", err)
	}
	return nil
}

// clearUploadsExcept removes every user's receipt directory but the one kept.
//
// Receipts live on disk under uploads/<user id>/, which no database cascade can
// reach, so they would otherwise survive their owner's account.
func clearUploadsExcept(dir string, keepID int64) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	mine := strconv.FormatInt(keepID, 10)
	removed := 0
	for _, e := range entries {
		if e.Name() == mine {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 {
		fmt.Printf("Removed %d other user's receipt folder(s).\n", removed)
	}
	return nil
}

// summarise prints a row count per table.
func summarise(sqlDB *sql.DB) error {
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
		fmt.Println("The database has no tables.")
		return nil
	}

	fmt.Println("Current contents:")
	for _, t := range tables {
		var n int
		// The table name cannot be a bound parameter, and it came from
		// sqlite_master rather than from user input, so quoting it is enough.
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM "` + t + `"`).Scan(&n); err != nil {
			fmt.Printf("  %-28s (unreadable)\n", t)
			continue
		}
		fmt.Printf("  %-28s %d rows\n", t, n)
	}
	return nil
}

// dropEverything removes every table and view.
//
// Dropping tables rather than deleting the file, because the file may be held
// open by a syncing client such as OneDrive, and because this leaves the
// database's own permissions and location untouched.
func dropEverything(sqlDB *sql.DB) error {
	// Foreign keys off for the duration. With them on, dropping a parent table
	// performs an implicit cascading DELETE, so the drop order would matter and
	// a wrong one would fail.
	//
	// PRAGMA is a no-op inside a transaction, so this is deliberately not run in
	// one; a partial drop is harmless here because the schema is rebuilt straight
	// afterwards.
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
			fmt.Printf("Dropped %d %s(s).\n", len(names), kind)
		}
	}

	// Reset the AUTOINCREMENT high-water marks so new ids start at 1.
	sqlDB.Exec(`DELETE FROM sqlite_sequence`)

	// Reclaim the space the dropped data occupied.
	if _, err := sqlDB.Exec(`VACUUM`); err != nil {
		fmt.Printf("(vacuum skipped: %v)\n", err)
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

// findAnomalies replays every household's ledger in order and reports movements
// that were impossible at the moment they were recorded.
//
// Replaying rather than checking the end state, because the end state cannot
// tell you which row is wrong. Household 8 currently holds a fifty-million
// dollar deposit against $2,500 of income; a query on final balances says only
// "this household is broken", while a replay names the transaction and the
// moment it went wrong.
//
// The rule is exactly the invariant the application now enforces: Deposit
// refuses to move more into a fund than the household has in cash, and Withdraw
// refuses to take out more than the fund holds. These rows predate those checks
// -- they came in through the legacy import, and one of them was created by the
// old delete-fund handler that credited a client-supplied balance.
//
// Note that this reports rather than decides. Deleting somebody's financial
// record on a heuristic is not a thing a program should do unasked, and this
// database proves why: two rows are flagged, and only one of them is the famous
// one.
func findAnomalies(sqlDB *sql.DB) ([]anomaly, error) {
	rows, err := sqlDB.Query(`
		SELECT household_id, id, kind, label, amount_cents, occurred_on, IFNULL(fund_id, 0)
		FROM transactions
		ORDER BY household_id, occurred_on, id`)
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

func runRepair(dbPath, backupDir, uploadDir, del string, yes bool) error {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist", dbPath)
	}
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
		fmt.Println("No impossible fund movements found.")
		return nil
	}

	fmt.Printf("%d fund movement(s) that could not have happened when they did:\n\n", len(found))
	byHousehold := map[int64][]int64{}
	for _, a := range found {
		fmt.Printf("  transaction %d  household %d  %s\n", a.TxID, a.Household, a.OccurredOn)
		fmt.Printf("      %s of %s labelled %q\n", a.Kind, a.Amount.Display(), a.Label)
		fmt.Printf("      but only %s was %s at that point\n\n",
			a.Available.Display(), a.AvailableOf)
		byHousehold[a.Household] = append(byHousehold[a.Household], a.TxID)
	}

	// What removing them would do, so the decision is an informed one.
	fmt.Println("Effect of removing them:")
	for hh, ids := range byHousehold {
		before, err := householdCash(sqlDB, hh, nil)
		if err != nil {
			return err
		}
		after, err := householdCash(sqlDB, hh, ids)
		if err != nil {
			return err
		}
		fmt.Printf("  household %d: cash %s → %s\n", hh, before.Display(), after.Display())
	}

	if del == "" {
		fmt.Println("\nNothing was changed. To remove specific rows:")
		fmt.Printf("  yaba repair -delete %s\n", joinIDs(allIDs(found)))
		fmt.Println("\nRemoving only some of them may leave the household still")
		fmt.Println("inconsistent, which is why the ids are yours to choose.")
		return nil
	}

	// Only ids this command reported are accepted. A typo would otherwise delete
	// an ordinary transaction, and there is no undo beyond the snapshot.
	wanted, err := parseIDs(del)
	if err != nil {
		return err
	}
	flagged := map[int64]anomaly{}
	for _, a := range found {
		flagged[a.TxID] = a
	}
	for _, id := range wanted {
		if _, ok := flagged[id]; !ok {
			return fmt.Errorf("transaction %d was not reported as impossible; "+
				"this command will only delete rows it flagged", id)
		}
	}

	fmt.Printf("\nAbout to permanently delete %d transaction(s): %s\n",
		len(wanted), joinIDs(wanted))
	if !yes {
		fmt.Print("Type 'delete' to continue: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "delete" {
			return errors.New("cancelled")
		}
	}

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
		Dir: backupDir, UploadDir: uploadDir,
	})
	if err != nil {
		return fmt.Errorf("backup failed, so nothing was deleted: %w", err)
	}
	fmt.Printf("Backed up to %s\n", snap.Path)

	// One transaction: either every named row goes or none does. A partial
	// deletion would leave the ledger in a state neither the old nor the new one.
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
	fmt.Printf("Deleted %d transaction(s).\n\n", len(wanted))

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
		fmt.Printf("  household %d now holds %s in cash\n", hh, c.Display())
	}
	if len(left) == 0 {
		fmt.Println("\nNo impossible movements remain.")
	} else {
		fmt.Printf("\n%d impossible movement(s) still present: %s\n",
			len(left), joinIDs(allIDs(left)))
		fmt.Println("Run this command again to see them.")
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
func runBackup(dbPath, dir, uploadDir string, keep int, list bool) error {
	if list {
		found, err := db.Snapshots(dir)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			fmt.Printf("No snapshots in %s\n", dir)
			return nil
		}
		fmt.Printf("%d snapshot(s) in %s, oldest first:\n", len(found), dir)
		for _, p := range found {
			size := int64(0)
			if info, err := os.Stat(p); err == nil {
				size = info.Size()
			}
			receipts := ""
			if _, err := os.Stat(strings.TrimSuffix(p, ".db") + "-uploads.zip"); err == nil {
				receipts = "  + receipts"
			}
			fmt.Printf("  %-34s %8.1f KiB%s\n", filepath.Base(p), float64(size)/1024, receipts)
		}
		return nil
	}

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist, so there is nothing to back up", dbPath)
	}

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	snap, err := db.Backup(context.Background(), sqlDB, db.BackupConfig{
		Dir: dir, UploadDir: uploadDir, Keep: keep,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Snapshot written and verified in %s\n", snap.Took.Round(time.Millisecond))
	fmt.Printf("  %s  (%.1f KiB)\n", snap.Path, float64(snap.Bytes)/1024)
	if snap.Uploads != "" {
		fmt.Printf("  %s\n", snap.Uploads)
	}
	for _, t := range []string{"users", "households", "transactions", "funds"} {
		fmt.Printf("  %-18s %d\n", t, snap.Counts[t])
	}
	if len(snap.Pruned) > 0 {
		fmt.Printf("Pruned %d old file(s), keeping the newest %d snapshots.\n",
			len(snap.Pruned), keep)
	}
	return nil
}

// runRestore puts a snapshot back, after proving it is usable.
//
// The default is the newest snapshot, because in the situation where this
// command gets typed for real -- the database is gone or damaged -- nobody wants
// to be reading filenames.
func runRestore(from, dir, dbPath string, force, check bool) error {
	if from == "" {
		found, err := db.Snapshots(dir)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no snapshots found in %s", dir)
		}
		from = found[len(found)-1]
		fmt.Printf("Using the newest snapshot: %s\n", from)
	}

	if check {
		counts, err := db.VerifySnapshot(context.Background(), from)
		if err != nil {
			return err
		}
		fmt.Printf("%s verifies.\n", filepath.Base(from))
		for _, t := range []string{"users", "households", "transactions", "funds"} {
			fmt.Printf("  %-18s %d\n", t, counts[t])
		}
		return nil
	}

	// Preserve whatever is currently there before overwriting it. A restore is
	// usually done under pressure, and "the file I just replaced turned out to
	// be the good one" is a common way for a bad hour to become a bad week.
	if _, err := os.Stat(dbPath); err == nil {
		aside := dbPath + ".before-restore-" + time.Now().UTC().Format("20060102-150405Z")
		if err := os.Rename(dbPath, aside); err != nil {
			return fmt.Errorf("could not move %s aside: %w", dbPath, err)
		}
		fmt.Printf("Moved the existing database to %s\n", aside)
		force = true
	}

	if err := db.Restore(context.Background(), from, dbPath, force); err != nil {
		return err
	}
	fmt.Printf("Restored %s to %s\n", filepath.Base(from), dbPath)
	fmt.Printf("Receipts are not restored automatically: unzip the matching "+
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
//
// Duplicated from the server's main rather than shared: the two binaries have no
// package in common below them, and a package existing only to hold one
// six-line helper is a worse trade than the duplication.
func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a positive integer; using %d\n",
			key, v, fallback)
		return fallback
	}
	return n
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}


// ═════════════════════════════════════════════════════════════════════════════
// _passwd.go
// ═════════════════════════════════════════════════════════════════════════════


func runPasswd(dbPath, email, password string, list bool) error {
	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// Deliberately not running migrations. This tool touches one column of one
	// row; silently upgrading someone's schema as a side effect of a password
	// reset would be a surprising and hard-to-undo thing for it to do. Run the
	// server once first if the database is older than the current schema.

	if list {
		return listAccounts(sqlDB)
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

	if password == "" {
		fmt.Printf("Setting a new password for %s (id %d).\n", current, id)
		password, err = prompt("New password: ")
		if err != nil {
			return err
		}
		again, err := prompt("Confirm password: ")
		if err != nil {
			return err
		}
		if password != again {
			return errors.New("the two passwords do not match")
		}
	}

	if err := checkPassword(password); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// The password and the session revocation go in one transaction. A password
	// changed without the sessions being dropped is the exact hole this exists to
	// close, so the two must not be able to come apart: if the DELETE fails, the
	// new password is rolled back too and the operator is told to retry.
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

	// Every device holding a session for this account is signed out. Before the
	// sessions table existed there was nothing to delete, and an old cookie kept
	// working after a password change -- which made changing it nearly pointless
	// as a response to a suspected compromise.
	revoked, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	n, _ := revoked.RowsAffected()

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Printf("Password updated for %s.\n", current)
	switch n {
	case 0:
		fmt.Println("No active logins to sign out.")
	case 1:
		fmt.Println("Signed out 1 active login. It will be asked to log in again.")
	default:
		fmt.Printf("Signed out %d active logins. They will be asked to log in again.\n", n)
	}
	return nil
}

func listAccounts(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), created_at
		FROM users ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	fmt.Printf("%-5s %-34s %-16s %s\n", "ID", "EMAIL", "NAME", "CREATED")
	for rows.Next() {
		var id int64
		var email, name, created string
		if err := rows.Scan(&id, &email, &name, &created); err != nil {
			return err
		}
		fmt.Printf("%-5d %-34s %-16s %s\n", id, email, name, created)
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

// prompt reads a line from stdin.
//
// The input is visible. Turning terminal echo off would mean importing
// golang.org/x/term, and adding a dependency for this is not worth it -- the
// alternative is documented instead: pass -password on a machine where nobody is
// looking over your shoulder, and remember it lands in your shell history.
func prompt(label string) (string, error) {
	fmt.Print(label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
