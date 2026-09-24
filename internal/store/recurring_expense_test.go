package store_test

// Tests for recurring expenses: the schedule that creates transactions, as
// opposed to a bucket, which is a monthly budget line and creates nothing.
//
// The three things worth pinning are the ones a user would notice going wrong:
// that catching up creates every occurrence owed rather than just the latest,
// that running it twice does not charge them twice, and that what the schedule
// creates is a real expense carrying the essential flag and the bucket it was
// given -- because those two are what the emergency-fund target is built from.

import (
	"context"
	"errors"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

func TestRecurringExpenseCatchesUpOnEveryDueOccurrence(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateRecurringExpense(
		ctx, sc, "Rent", 120000, nil, true, 1, "month", "2026-01-01",
	); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Nobody opened the page until April. January, February, March and April are
	// all owed by then -- four charges, not one.
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-04-15"); err != nil {
		t.Fatalf("process: %v", err)
	}

	txs, _, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(txs) != 4 {
		t.Fatalf("got %d transactions, want 4 (Jan–Apr)", len(txs))
	}

	seen := map[string]bool{}
	for _, tx := range txs {
		seen[tx.OccurredOn] = true
		if tx.Kind != store.KindExpense {
			t.Errorf("%s: kind = %q, want expense", tx.OccurredOn, tx.Kind)
		}
		if tx.Amount != 120000 {
			t.Errorf("%s: amount = %d, want 120000", tx.OccurredOn, tx.Amount)
		}
	}
	for _, want := range []string{"2026-01-01", "2026-02-01", "2026-03-01", "2026-04-01"} {
		if !seen[want] {
			t.Errorf("no transaction on %s", want)
		}
	}
}

// Every page load runs the catch-up, so running it twice has to be free. The
// UNIQUE(schedule, due_date) constraint is what makes it so; this test is what
// notices if that constraint is ever dropped.
func TestRecurringExpenseDoesNotChargeTwice(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateRecurringExpense(
		ctx, sc, "Gym", 4500, nil, false, 2, "week", "2026-03-02",
	); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-03-30"); err != nil {
			t.Fatalf("process %d: %v", i, err)
		}
	}

	txs, _, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 2 Mar, 16 Mar, 30 Mar.
	if len(txs) != 3 {
		t.Fatalf("got %d transactions after three runs, want 3", len(txs))
	}
}

// A schedule is not just an amount: automating rent must not quietly drop it out
// of the essential spending that sizes the emergency fund, or out of the bucket
// it was paying towards.
func TestRecurringExpenseCarriesEssentialAndBucket(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	bucket, err := st.CreateBucket(ctx, sc, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 120000,
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if _, err := st.CreateRecurringExpense(
		ctx, sc, "Rent", 120000, &bucket, true, 1, "month", "2026-05-01",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-05-01"); err != nil {
		t.Fatalf("process: %v", err)
	}

	txs, _, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("got %d transactions, want 1", len(txs))
	}
	got := txs[0]
	if got.Essential == nil || !*got.Essential {
		t.Error("generated expense is not essential")
	}
	if got.BucketID == nil || *got.BucketID != bucket {
		t.Errorf("BucketID = %v, want %d", got.BucketID, bucket)
	}
}

// Nothing is owed before the start date, so choosing a future first charge does
// not immediately record one.
func TestRecurringExpenseWaitsForItsStartDate(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateRecurringExpense(
		ctx, sc, "Insurance", 8000, nil, true, 1, "month", "2026-09-01",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-08-31"); err != nil {
		t.Fatalf("process: %v", err)
	}

	_, total, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 {
		t.Fatalf("got %d transactions before the start date, want 0", total)
	}
}

// Bad input is refused at the store, not only in the handler: the handler is one
// caller, and a second one must not be able to store "every 0 months".
func TestRecurringExpenseRejectsNonsense(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		n    int
		unit string
		amt  store.Cents
	}{
		{"zero interval", 0, "month", 1000},
		{"negative interval", -2, "week", 1000},
		{"unknown unit", 1, "fortnight", 1000},
		{"zero amount", 1, "month", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := st.CreateRecurringExpense(
				ctx, sc, "Nope", c.amt, nil, false, c.n, c.unit, "2026-01-01",
			); err == nil {
				t.Fatal("created a schedule that should have been refused")
			}
		})
	}
}

// ── frequency bounds ──────────────────────────────────────────────────────────

// TestFrequencyCeilingsStopTheOverflow is the regression test for two ways a
// single POST could take a household's pages down permanently.
//
// frequency_n was bounded only by "> 0" and was then multiplied:
//
//   - every 4611686018427387904 weeks: frequencyN*7 wraps int64 back to a
//     multiple of a year, AddDate returns the SAME date, and the catch-up loop
//     never terminates -- while holding the process's single database
//     connection, so every other user's request blocks behind it.
//
//   - every 2000000000 months: AddDate returns "166668693-05-17", which is
//     written onto a real transaction and then cannot be re-parsed, so
//     next_due_date can never advance and the page 500s from then on.
func TestFrequencyCeilingsStopTheOverflow(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	for _, c := range []struct {
		name string
		n    int
		unit string
	}{
		{"week overflow", 4611686018427387904, "week"},
		{"month overflow", 2000000000, "month"},
		{"day overflow", 1 << 62, "day"},
		{"just over the day ceiling", 366, "day"},
		{"just over the week ceiling", 53, "week"},
		{"just over the month ceiling", 121, "month"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := st.CreateRecurringExpense(
				ctx, sc, "Boom", 1000, nil, false, c.n, c.unit, "2026-01-01",
			); err == nil {
				t.Fatal("the schedule was accepted")
			}
			if _, err := st.CreateRecurringIncome(
				ctx, sc, "Boom", 1000, c.n, c.unit, "2026-01-01",
			); err == nil {
				t.Fatal("the income schedule was accepted")
			}
		})
	}
}

// The ceilings must not refuse a schedule somebody would actually set.
func TestFrequencyCeilingsAllowRealSchedules(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	for _, c := range []struct {
		n    int
		unit string
	}{
		{1, "day"}, {14, "day"}, {365, "day"},
		{1, "week"}, {2, "week"}, {52, "week"},
		{1, "month"}, {3, "month"}, {12, "month"}, {120, "month"},
	} {
		if _, err := st.CreateRecurringExpense(
			ctx, sc, "Fine", 1000, nil, false, c.n, c.unit, "2026-01-01",
		); err != nil {
			t.Errorf("every %d %s was refused: %v", c.n, c.unit, err)
		}
	}
}

// Catching up is bounded so one very old schedule cannot monopolise the single
// database connection. The rest is generated on the next visit, and nothing is
// skipped: the dates resume exactly where they stopped.
func TestCatchUpIsBoundedAndResumes(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateRecurringExpense(
		ctx, sc, "Daily", 100, nil, false, 1, "day", "2020-01-01",
	); err != nil {
		t.Fatal(err)
	}

	// 2020-01-01 to 2026-01-01 is ~2192 days, comfortably over the cap.
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-01-01"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	_, first, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("the first pass generated nothing")
	}
	if first > 500 {
		t.Fatalf("the first pass generated %d occurrences, want at most 500", first)
	}

	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-01-01"); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	_, second, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("the second pass added nothing (%d then %d) — the catch-up does not resume", first, second)
	}

	// No duplicates: every generated date is distinct.
	txs, _, err := st.List(ctx, sc, store.Filter{Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tx := range txs {
		if seen[tx.OccurredOn] {
			t.Fatalf("two transactions on %s — the catch-up double-charged", tx.OccurredOn)
		}
		seen[tx.OccurredOn] = true
	}
}

// ── managing a schedule after it is created ──────────────────────────────────

// An edit changes what future occurrences carry, and leaves the next due date
// -- and everything already recorded -- exactly where it was.
func TestRecurringExpenseUpdateAppliesToFutureOccurrences(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateRecurringExpense(ctx, sc, "Phone", 3000, nil, false, 1, "month", "2026-01-10")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-01-10"); err != nil {
		t.Fatal(err)
	}
	before, err := st.RecurringExpenseByID(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}

	bucket, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Bills", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRecurringExpense(ctx, sc, id, "Mobile", 4500, &bucket, true, 2, "week"); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := st.RecurringExpenseByID(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Label != "Mobile" || after.Amount != 4500 || !after.Essential ||
		after.FrequencyN != 2 || after.FrequencyUnit != "week" {
		t.Errorf("update not applied: %+v", after)
	}
	if after.BucketRef() != bucket || after.BucketName != "Bills" {
		t.Errorf("bucket = %d %q, want %d \"Bills\"", after.BucketRef(), after.BucketName, bucket)
	}
	if after.NextDueDate != before.NextDueDate {
		t.Errorf("next due moved from %s to %s", before.NextDueDate, after.NextDueDate)
	}

	// The January charge already recorded is untouched.
	txs, _, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 || txs[0].Label != "Phone" || txs[0].Amount != 3000 {
		t.Errorf("history was rewritten: %+v", txs)
	}
}

func TestRecurringExpenseUpdateRefusesNonsense(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id, err := st.CreateRecurringExpense(ctx, sc, "Phone", 3000, nil, false, 1, "month", "2026-01-10")
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"blank name":  func() error { return st.UpdateRecurringExpense(ctx, sc, id, "  ", 100, nil, false, 1, "month") },
		"zero amount": func() error { return st.UpdateRecurringExpense(ctx, sc, id, "X", 0, nil, false, 1, "month") },
		"overflow":    func() error { return st.UpdateRecurringExpense(ctx, sc, id, "X", 100, nil, false, 2000000000, "month") },
		"unknown bucket": func() error {
			b := int64(999999)
			return st.UpdateRecurringExpense(ctx, sc, id, "X", 100, &b, false, 1, "month")
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Cancelling stops future charges and keeps the ones already made.
func TestCancelledRecurringExpenseStopsCharging(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id, err := st.CreateRecurringExpense(ctx, sc, "Gym", 4000, nil, false, 1, "month", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-02-15"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRecurringExpenseActive(ctx, sc, id, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-06-15"); err != nil {
		t.Fatal(err)
	}
	_, total, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("%d charges, want the 2 made before cancelling", total)
	}
	// A cancelled schedule can no longer be edited.
	if err := st.UpdateRecurringExpense(ctx, sc, id, "Gym", 5000, nil, false, 1, "month"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("editing a cancelled schedule: %v, want ErrNotFound", err)
	}
}

func TestRecurringExpenseIsScopedToTheHousehold(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()
	id, err := st.CreateRecurringExpense(ctx, alice, "Rent", 90000, nil, true, 1, "month", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecurringExpenseByID(ctx, bob, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob read alice's schedule: %v", err)
	}
	if err := st.UpdateRecurringExpense(ctx, bob, id, "Hacked", 1, nil, false, 1, "month"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob edited alice's schedule: %v", err)
	}
	if err := st.SetRecurringExpenseActive(ctx, bob, id, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob cancelled alice's schedule: %v", err)
	}
}
