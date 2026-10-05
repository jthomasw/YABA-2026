package store_test

// Tests for the month arithmetic behind recurring schedules.
//
// The bug: each due date used to be the previous one plus AddDate(0, n, 0), and
// Go normalises an impossible date forwards, so Jan 31 + 1 month is Mar 3. A
// schedule starting on the 31st then ran Jan 31, Mar 3, Apr 3 ... for ever:
// February was never charged and every later date was on the wrong day.

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// walk returns the n occurrences after anchor, each computed from the last, the
// way the catch-up loops compute them.
func walk(t *testing.T, anchor string, n int, freqN int, unit string) []string {
	t.Helper()
	out := make([]string, 0, n)
	cur := anchor
	for i := 0; i < n; i++ {
		next, err := store.AdvanceRecurringDate(anchor, cur, freqN, unit)
		if err != nil {
			t.Fatalf("advance from %s: %v", cur, err)
		}
		out = append(out, next)
		cur = next
	}
	return out
}

func TestAdvanceRecurringDateKeepsTheAnchorDay(t *testing.T) {
	tests := []struct {
		name   string
		anchor string
		freqN  int
		unit   string
		want   []string
	}{
		{
			// 2027 is a common year and 2028 a leap year, so both Februaries
			// are crossed and every month length appears.
			name: "31st monthly through a common and a leap year", anchor: "2027-01-31",
			freqN: 1, unit: "month",
			want: []string{
				"2027-02-28", "2027-03-31", "2027-04-30", "2027-05-31", "2027-06-30",
				"2027-07-31", "2027-08-31", "2027-09-30", "2027-10-31", "2027-11-30",
				"2027-12-31", "2028-01-31", "2028-02-29", "2028-03-31", "2028-04-30",
			},
		},
		{
			name: "31 Aug quarterly", anchor: "2023-08-31", freqN: 3, unit: "month",
			want: []string{
				"2023-11-30", "2024-02-29", "2024-05-31", "2024-08-31",
				"2024-11-30", "2025-02-28", "2025-05-31",
			},
		},
		{
			// Back on the 29th the moment a leap year comes round, rather than
			// stuck on the 28th (or worse, 1 March) for good.
			name: "29 Feb every 12 months", anchor: "2024-02-29", freqN: 12, unit: "month",
			want: []string{"2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29", "2029-02-28"},
		},
		{
			name: "30th monthly", anchor: "2026-01-30", freqN: 1, unit: "month",
			want: []string{"2026-02-28", "2026-03-30", "2026-04-30", "2026-05-30"},
		},
		{
			name: "a day every month has is untouched", anchor: "2026-01-15", freqN: 1, unit: "month",
			want: []string{"2026-02-15", "2026-03-15", "2026-04-15"},
		},
		{
			// Days and weeks have no month edge, and step exactly as before.
			name: "daily", anchor: "2026-01-31", freqN: 1, unit: "day",
			want: []string{"2026-02-01", "2026-02-02"},
		},
		{
			name: "fortnightly", anchor: "2026-01-31", freqN: 2, unit: "week",
			want: []string{"2026-02-14", "2026-02-28", "2026-03-14"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := walk(t, tt.anchor, len(tt.want), tt.freqN, tt.unit)
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("occurrence %d = %s, want %s (all: %v)", i+1, got[i], tt.want[i], got)
				}
			}
		})
	}
}

// transactionDates lists the dates of every transaction in the household,
// oldest first.
func transactionDates(t *testing.T, st *store.Store, sc store.Scope) []string {
	t.Helper()
	txs, _, err := st.List(context.Background(), sc, store.Filter{Limit: 500})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.OccurredOn
	}
	sort.Strings(out)
	return out
}

// TestRecurringExpenseOnThe31stChargesEveryMonth is the bug as the user saw it:
// rent due on the 31st must be charged in February too, and back on the 31st
// in March.
func TestRecurringExpenseOnThe31stChargesEveryMonth(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateRecurringExpense(ctx, sc, "Rent", 100000, nil, true, 1, "month", "2027-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2028-04-30"); err != nil {
		t.Fatal(err)
	}

	want := []string{"2027-12-31", "2028-01-31", "2028-02-29", "2028-03-31", "2028-04-30"}
	got := transactionDates(t, st, sc)
	if len(got) != len(want) {
		t.Fatalf("dates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dates = %v, want %v", got, want)
		}
	}

	r, err := st.RecurringExpenseByID(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.NextDueDate != "2028-05-31" {
		t.Errorf("next due = %s, want 2028-05-31", r.NextDueDate)
	}
}

// drifted opens a migrated database for the migration-19 tests, keeping the
// raw handle so a test can store what the old arithmetic left behind and then
// replay the repair.
func drifted(t *testing.T) (*store.Store, store.Scope, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "drift.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(sqlDB)
	ctx := context.Background()
	uid, err := st.CreateUser(ctx, "drift@example.com", "not-a-real-hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hh, err := st.ActiveHousehold(ctx, store.User{ID: uid})
	if err != nil {
		t.Fatalf("active household: %v", err)
	}
	return st, store.Scope{HouseholdID: hh.ID, UserID: uid}, sqlDB
}

// replayRepair runs migration 19 again over the current rows, exactly as an
// upgrade would run it over a database written by the old binary.
func replayRepair(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	if _, err := sqlDB.Exec(`DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
		t.Fatalf("rewind schema_migrations: %v", err)
	}
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestMigrationRepairsDriftedSchedules is the repair's rule as a table: only
// the dates the old arithmetic produced move, and only within their month.
func TestMigrationRepairsDriftedSchedules(t *testing.T) {
	tests := []struct {
		name, anchor, due, unit, want string
	}{
		{"drifted 31st", "2025-01-31", "2025-06-03", "month", "2025-06-30"},
		{"drifted 31st into a 31-day month", "2025-01-31", "2025-07-03", "month", "2025-07-31"},
		{"drifted 30th", "2025-01-30", "2025-04-02", "month", "2025-04-30"},
		{"drifted 29th, common year", "2025-01-29", "2025-03-01", "month", "2025-03-29"},
		{"drifted 31st into February", "2025-12-31", "2026-02-03", "month", "2026-02-28"},

		{"on schedule", "2025-01-31", "2025-04-30", "month", "2025-04-30"},
		{"clamped February", "2025-01-31", "2025-02-28", "month", "2025-02-28"},
		{"anchor day drift cannot touch", "2025-01-15", "2025-06-03", "month", "2025-06-03"},
		{"due day drift cannot produce", "2025-01-31", "2025-06-10", "month", "2025-06-10"},
		{"weekly", "2025-01-31", "2025-06-03", "week", "2025-06-03"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, sc, sqlDB := drifted(t)
			ctx := context.Background()
			id, err := st.CreateRecurringExpense(ctx, sc, "Rent", 100000, nil, true, 1, tt.unit, tt.anchor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sqlDB.Exec(`UPDATE recurring_expense SET next_due_date = ? WHERE id = ?`, tt.due, id); err != nil {
				t.Fatal(err)
			}
			replayRepair(t, sqlDB)
			r, err := st.RecurringExpenseByID(ctx, sc, id)
			if err != nil {
				t.Fatal(err)
			}
			if r.NextDueDate != tt.want {
				t.Errorf("next due after repair = %s, want %s", r.NextDueDate, tt.want)
			}
		})
	}
}

// TestDriftedRecurringIncomeIsRealignedWithoutDoublePosting: a schedule stored
// before the fix, which ran Jan 31 and was left due on Mar 3, goes back to the
// 31st with one payment a month from then on.
func TestDriftedRecurringIncomeIsRealignedWithoutDoublePosting(t *testing.T) {
	st, sc, sqlDB := drifted(t)
	ctx := context.Background()

	id, err := st.CreateRecurringIncome(ctx, sc, "Salary", 300000, 1, "month", "2025-01-31")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringIncome(ctx, sc, "2025-01-31"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE recurring_income SET next_due_date = '2025-03-03' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	replayRepair(t, sqlDB)

	for _, asOf := range []string{"2025-03-31", "2025-04-15", "2025-06-30", "2025-06-30"} {
		if err := st.ProcessDueRecurringIncome(ctx, sc, asOf); err != nil {
			t.Fatal(err)
		}
	}
	assertDates(t, transactionDates(t, st, sc),
		[]string{"2025-01-31", "2025-03-31", "2025-04-30", "2025-05-31", "2025-06-30"})
}

// TestAnAlreadyPostedDriftedDateIsNotMovedAgain: the old catch-up posted each
// occurrence in its own transaction and saved next_due_date only at the end,
// so an interrupted run could leave next_due_date on a date already posted.
// Moving that date would charge its month twice; the repair leaves it, and
// the catch-up skips it and steps on with the fixed arithmetic.
func TestAnAlreadyPostedDriftedDateIsNotMovedAgain(t *testing.T) {
	st, sc, sqlDB := drifted(t)
	ctx := context.Background()

	id, err := st.CreateRecurringExpense(ctx, sc, "Rent", 100000, nil, true, 1, "month", "2025-01-31")
	if err != nil {
		t.Fatal(err)
	}
	// Post Jan 31 and the drifted Mar 3, then leave next_due_date on Mar 3, as
	// an interrupted old catch-up would have.
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2025-01-31"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE recurring_expense SET next_due_date = '2025-03-03' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2025-03-03"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE recurring_expense SET next_due_date = '2025-03-03' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	replayRepair(t, sqlDB)
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2025-04-30"); err != nil {
		t.Fatal(err)
	}
	assertDates(t, transactionDates(t, st, sc),
		[]string{"2025-01-31", "2025-03-03", "2025-04-30"})
}

// assertDates fails unless got and want hold the same dates in order.
func assertDates(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("dates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dates = %v, want %v", got, want)
		}
	}
}

// TestChangingTheIntervalStepsFromTheDueDate: an edit can change "every month"
// to "every 2 months" without moving the next due date, and the next step must
// then be two months on from that date, on the anchor's day.
func TestChangingTheIntervalStepsFromTheDueDate(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateRecurringExpense(ctx, sc, "Water", 5000, nil, true, 1, "month", "2026-01-31")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-03-31"); err != nil {
		t.Fatal(err)
	}
	// Jan 31, Feb 28, Mar 31 posted; Apr 30 is next.
	if err := st.UpdateRecurringExpense(ctx, sc, id, "Water", 5000, nil, true, 2, "month"); err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-08-31"); err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-06-30", "2026-08-31"}
	got := transactionDates(t, st, sc)
	if len(got) != len(want) {
		t.Fatalf("dates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dates = %v, want %v", got, want)
		}
	}
}
