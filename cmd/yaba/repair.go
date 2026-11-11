package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/money"
)

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
