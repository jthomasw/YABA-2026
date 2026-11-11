package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/db"
)

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
	// still physically stored in the uploader's directory. Preserve every
	// directory referenced by a surviving row instead of assuming keepID owns
	// it: saved expenses (transactions.receipt_path) and receipts uploaded but
	// not yet entered (receipt_jobs.path), which survive the prune too.
	keep := map[string]bool{strconv.FormatInt(keepID, 10): true}
	var unplaced []string
	for _, q := range []string{
		`SELECT receipt_path FROM transactions WHERE receipt_path IS NOT NULL AND receipt_path <> ''`,
		`SELECT path FROM receipt_jobs WHERE path <> ''`,
	} {
		paths, err := queryStrings(sqlDB, q)
		if err != nil {
			return fmt.Errorf("list surviving receipt paths: %w", err)
		}
		for _, stored := range paths {
			first, ok := uploadFolderOf(dir, stored)
			if !ok {
				unplaced = append(unplaced, stored)
				continue
			}
			keep[first] = true
		}
	}
	// A path that can't be placed under the uploads folder (the server stored
	// it relative to a different working directory, say) could belong to any
	// of the folders about to be removed. Deleting nothing is recoverable;
	// deleting a receipt somebody still needs is not.
	if len(unplaced) > 0 {
		fmt.Fprintf(a.out, "Left %s untouched: %d surviving receipt path(s) such as %q are not inside it, so it can't tell which folders are still needed.\n",
			dir, len(unplaced), unplaced[0])
		return nil
	}
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

// queryStrings runs a one-column query and returns every value.
func queryStrings(sqlDB *sql.DB, q string) ([]string, error) {
	rows, err := sqlDB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// uploadFolderOf names the folder directly under the uploads directory that a
// stored receipt path lives in ("2" for uploads/2/abc.jpg). The server stores
// paths relative to its own working directory, which need not be this
// command's, and -uploads may be absolute when they are relative: so a path is
// tried as given (made absolute), and then, when it starts with the uploads
// folder's own name, as relative to that folder.
func uploadFolderOf(dir, stored string) (string, bool) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	p := filepath.FromSlash(stored)
	try := []string{p}
	if !filepath.IsAbs(p) {
		parts := strings.Split(filepath.Clean(p), string(os.PathSeparator))
		if len(parts) > 1 && parts[0] == filepath.Base(absDir) {
			try = append(try, filepath.Join(append([]string{absDir}, parts[1:]...)...))
		}
	}
	for _, c := range try {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absDir, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		first := strings.Split(rel, string(os.PathSeparator))[0]
		if first != "" && first != "." {
			return first, true
		}
	}
	return "", false
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
