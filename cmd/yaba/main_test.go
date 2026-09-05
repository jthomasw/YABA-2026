// Tests for the repair diagnosis.
//
// The subject is a judgement about somebody's money, and the consequence of
// getting it wrong is a deleted financial record. So these cover both
// directions: that an impossible movement is caught, and — more importantly —
// that ordinary activity is not.
package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/db"
)

func newLedger(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := sqlDB.Exec(`
		INSERT INTO users(id, username, email, display_name, password_hash)
		     VALUES(1, 'a', 'a@example.com', 'A', 'hash');
		INSERT INTO households(id, name, personal_for) VALUES(1, 'Mine', 1);
		INSERT INTO household_members(household_id, user_id, role) VALUES(1, 1, 'owner');
		INSERT INTO funds(id, user_id, household_id, name) VALUES(1, 1, 1, 'Rainy day');`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return sqlDB
}

// add books one movement. Dates are given explicitly because the replay orders
// by date, and half of what it checks is sequence.
func add(t *testing.T, sqlDB *sql.DB, kind string, cents int64, date string) int64 {
	t.Helper()
	var fund any
	if kind == "fund_deposit" || kind == "fund_withdrawal" {
		fund = 1
	}
	res, err := sqlDB.Exec(`
		INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on, fund_id)
		VALUES(1, 1, ?, 'x', ?, ?, ?)`, kind, cents, date, fund)
	if err != nil {
		t.Fatalf("insert %s: %v", kind, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestOrdinaryActivityIsNotFlagged(t *testing.T) {
	sqlDB := newLedger(t)

	add(t, sqlDB, "income", 100000, "2026-01-01")         // $1,000 in
	add(t, sqlDB, "expense", 20000, "2026-01-02")         //   $200 out
	add(t, sqlDB, "fund_deposit", 50000, "2026-01-03")    //   $500 saved
	add(t, sqlDB, "fund_withdrawal", 50000, "2026-01-04") // and taken back

	found, err := findAnomalies(sqlDB)
	if err != nil {
		t.Fatalf("findAnomalies: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("flagged %d ordinary movement(s): %+v", len(found), found)
	}
}

// TestADepositBeyondCashIsFlagged is the shape of the row in the live database.
func TestADepositBeyondCashIsFlagged(t *testing.T) {
	sqlDB := newLedger(t)

	add(t, sqlDB, "income", 250000, "2026-04-22") // $2,500 of income ever
	bad := add(t, sqlDB, "fund_deposit", 5_000_000_000, "2026-04-23")

	found, err := findAnomalies(sqlDB)
	if err != nil {
		t.Fatalf("findAnomalies: %v", err)
	}
	if len(found) != 1 || found[0].TxID != bad {
		t.Fatalf("want transaction %d flagged, got %+v", bad, found)
	}
	if found[0].Available != 250000 {
		t.Errorf("available was reported as %s, want $2,500.00", found[0].Available.Display())
	}
}

// TestAWithdrawalBeyondTheFundIsFlagged: the other half of the invariant.
func TestAWithdrawalBeyondTheFundIsFlagged(t *testing.T) {
	sqlDB := newLedger(t)

	add(t, sqlDB, "income", 100000, "2026-01-01")
	add(t, sqlDB, "fund_deposit", 10000, "2026-01-02") // $100 in the fund
	bad := add(t, sqlDB, "fund_withdrawal", 90000, "2026-01-03")

	found, err := findAnomalies(sqlDB)
	if err != nil {
		t.Fatalf("findAnomalies: %v", err)
	}
	if len(found) != 1 || found[0].TxID != bad {
		t.Fatalf("want the withdrawal flagged, got %+v", found)
	}
	if found[0].AvailableOf != "in that fund" {
		t.Errorf("the message measures the wrong thing: %q", found[0].AvailableOf)
	}
}

// TestOrderIsByDateNotInsertion: money arriving before it is spent is fine even
// when the rows were entered the other way round, which is what backdating an
// income does.
func TestOrderIsByDateNotInsertion(t *testing.T) {
	sqlDB := newLedger(t)

	add(t, sqlDB, "fund_deposit", 50000, "2026-01-05") // entered first...
	add(t, sqlDB, "income", 100000, "2026-01-01")      // ...but dated earlier

	found, err := findAnomalies(sqlDB)
	if err != nil {
		t.Fatalf("findAnomalies: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a backdated income was ignored: %+v", found)
	}
}

// TestHouseholdsAreCountedSeparately: one budget's cash must not excuse another
// budget's deposit.
func TestHouseholdsAreCountedSeparately(t *testing.T) {
	sqlDB := newLedger(t)
	if _, err := sqlDB.Exec(`
		INSERT INTO users(id, username, email, display_name, password_hash)
		     VALUES(2, 'b', 'b@example.com', 'B', 'hash');
		INSERT INTO households(id, name, personal_for) VALUES(2, 'Theirs', 2);
		INSERT INTO household_members(household_id, user_id, role) VALUES(2, 2, 'owner');
		INSERT INTO funds(id, user_id, household_id, name) VALUES(2, 2, 2, 'Theirs');`,
	); err != nil {
		t.Fatalf("seed second household: %v", err)
	}

	add(t, sqlDB, "income", 1000000, "2026-01-01") // household 1 is rich
	if _, err := sqlDB.Exec(`
		INSERT INTO transactions(user_id, household_id, kind, label, amount_cents, occurred_on, fund_id)
		VALUES(2, 2, 'fund_deposit', 'x', 50000, '2026-01-02', 2)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	found, err := findAnomalies(sqlDB)
	if err != nil {
		t.Fatalf("findAnomalies: %v", err)
	}
	if len(found) != 1 || found[0].Household != 2 {
		t.Errorf("want household 2's deposit flagged, got %+v", found)
	}
}

// TestHouseholdCashIgnoresTheRowsBeingRemoved backs the "cash X → Y" line the
// command prints before asking for confirmation.
func TestHouseholdCashIgnoresTheRowsBeingRemoved(t *testing.T) {
	sqlDB := newLedger(t)

	add(t, sqlDB, "income", 250000, "2026-04-22")
	bad := add(t, sqlDB, "fund_deposit", 5_000_000_000, "2026-04-23")

	before, err := householdCash(sqlDB, 1, nil)
	if err != nil {
		t.Fatalf("cash: %v", err)
	}
	if before >= 0 {
		t.Errorf("cash is %s, expected it to be impossible", before.Display())
	}

	after, err := householdCash(sqlDB, 1, []int64{bad})
	if err != nil {
		t.Fatalf("cash ignoring: %v", err)
	}
	if after != 250000 {
		t.Errorf("cash without the bad row is %s, want $2,500.00", after.Display())
	}
}

func TestParseIDs(t *testing.T) {
	got, err := parseIDs(" 35, 36 ,")
	if err != nil {
		t.Fatalf("parseIDs: %v", err)
	}
	if len(got) != 2 || got[0] != 35 || got[1] != 36 {
		t.Errorf("got %v, want [35 36]", got)
	}
	if _, err := parseIDs("36,banana"); err == nil {
		t.Error("parseIDs accepted a non-number")
	}
	if _, err := parseIDs(""); err == nil {
		t.Error("parseIDs accepted an empty list")
	}
}
