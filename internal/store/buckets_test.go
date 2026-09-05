package store_test

// Characterisation tests for the recurring-expense buckets and the funding
// waterfall. These pin the behaviour of Buckets and Reallocate exactly as it
// stood before the query behind them was simplified, so the simplification can
// be proved to change nothing.

import (
	"context"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

func bucketExpense(t *testing.T, st *store.Store, sc store.Scope, bucketID int64, amount money.Cents, date string) {
	t.Helper()
	_, err := st.Add(context.Background(), sc, store.NewTransaction{
		Kind: store.KindExpense, Label: "Utilities", Amount: amount,
		OccurredOn: date, BucketID: &bucketID,
	})
	if err != nil {
		t.Fatalf("add bucket expense: %v", err)
	}
}

func bucketByID(t *testing.T, buckets []store.Bucket, id int64) store.Bucket {
	t.Helper()
	for _, b := range buckets {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("bucket %d not in list", id)
	return store.Bucket{}
}

// TestVariableBucketEstimateIsTheTrailingSixMonths: the estimate, low and high
// come from the six most recent months WITH activity before the month being
// viewed, so an older seventh month and the current month are both ignored.
func TestVariableBucketEstimateIsTheTrailingSixMonths(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	water, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Water", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	rent, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Rent", CostKind: store.CostFixed, Fixed: 120000})
	if err != nil {
		t.Fatal(err)
	}

	// Seven months of history before the viewed month, plus the viewed month.
	// The oldest (January, 9000) must fall outside the six-month window.
	history := map[string]money.Cents{
		"2026-01-10": 9000, // 7th most recent: excluded
		"2026-02-10": 3000, // the low
		"2026-03-10": 4000,
		"2026-04-10": 5000,
		"2026-05-10": 6000,
		"2026-06-10": 7000,
		"2026-07-10": 8000, // the high
	}
	for date, amt := range history {
		bucketExpense(t, st, sc, water, amt, date)
	}
	// Two payments in one month count as one month with their sum.
	bucketExpense(t, st, sc, water, 2000, "2026-07-20") // July becomes 10000

	buckets, err := st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	w := bucketByID(t, buckets, water)

	// (3000+4000+5000+6000+7000+10000)/6 = 5833.33, rounded to 5833
	if w.Estimate != 5833 {
		t.Errorf("estimate = %d, want 5833 (mean of the last six months)", w.Estimate)
	}
	if w.Low != 3000 || w.High != 10000 {
		t.Errorf("range = %d..%d, want 3000..10000", w.Low, w.High)
	}
	if w.Spent != 0 {
		t.Errorf("spent this month = %d, want 0", w.Spent)
	}
	if w.Due != w.Estimate {
		t.Errorf("with nothing spent yet, due = %d should equal the estimate %d", w.Due, w.Estimate)
	}

	r := bucketByID(t, buckets, rent)
	if r.Estimate != 0 || r.Low != 120000 || r.High != 120000 || r.Due != 120000 {
		t.Errorf("fixed bucket: estimate=%d low=%d high=%d due=%d, want 0/120000/120000/120000",
			r.Estimate, r.Low, r.High, r.Due)
	}
}

// TestVariableBucketSpentOverridesEstimate: once something is paid in the viewed
// month, that is what the bucket needs, and it widens the historical range.
func TestVariableBucketSpentOverridesEstimate(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	water, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Water", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	bucketExpense(t, st, sc, water, 4000, "2026-06-10")
	bucketExpense(t, st, sc, water, 6000, "2026-07-10")
	bucketExpense(t, st, sc, water, 9500, "2026-08-05") // this month, above the high

	buckets, err := st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	w := bucketByID(t, buckets, water)

	if w.Estimate != 5000 || w.Spent != 9500 || w.Due != 9500 {
		t.Errorf("estimate=%d spent=%d due=%d, want 5000/9500/9500", w.Estimate, w.Spent, w.Due)
	}
	if w.Low != 4000 || w.High != 9500 {
		t.Errorf("range = %d..%d, want 4000..9500 (spent stretches the high)", w.Low, w.High)
	}
}

// TestVariableBucketWithNoHistoryCollapsesToZero: no history, nothing spent.
func TestVariableBucketWithNoHistoryCollapsesToZero(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	water, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Water", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	w := bucketByID(t, buckets, water)
	if w.Estimate != 0 || w.Low != 0 || w.High != 0 || w.Due != 0 || w.Status() != "empty" {
		t.Errorf("no history: estimate=%d low=%d high=%d due=%d status=%q",
			w.Estimate, w.Low, w.High, w.Due, w.Status())
	}
}

// TestWaterfallPoursIncomeDownThePriorityList pins Reallocate: income is
// replayed oldest first, each payday fills buckets top to bottom, and a
// variable bucket is funded to its trailing estimate when nothing is spent yet.
func TestWaterfallPoursIncomeDownThePriorityList(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	rent, _ := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Rent", CostKind: store.CostFixed, Fixed: 100000})
	water, _ := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Water", CostKind: store.CostVariable})
	phone, _ := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Phone", CostKind: store.CostFixed, Fixed: 5000})

	// Water averaged 4000 over the two previous months.
	bucketExpense(t, st, sc, water, 3000, "2026-06-10")
	bucketExpense(t, st, sc, water, 5000, "2026-07-10")

	// Two paydays. The first cannot cover rent alone; the second finishes rent,
	// funds water in full, and leaves phone partly funded.
	addIncome(t, st, sc, 60000, "Pay 1", "2026-08-01")
	addIncome(t, st, sc, 46000, "Pay 2", "2026-08-15")

	if err := st.Reallocate(ctx, sc, "2026-08"); err != nil {
		t.Fatal(err)
	}
	buckets, err := st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}

	want := map[int64][2]money.Cents{ // allocated, due
		rent:  {100000, 100000},
		water: {4000, 4000},
		phone: {2000, 5000},
	}
	for id, w := range want {
		b := bucketByID(t, buckets, id)
		if b.Allocated != w[0] || b.Due != w[1] {
			t.Errorf("%s: allocated=%d due=%d, want %d/%d", b.Name, b.Allocated, b.Due, w[0], w[1])
		}
	}

	sum, err := st.AllocationsFor(ctx, sc, "2026-08", buckets)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Income != 106000 || sum.Allocated != 106000 || sum.Required != 109000 ||
		sum.Shortfall != 3000 || sum.Unassigned != 0 {
		t.Errorf("summary = %+v", sum)
	}

	// Which income funded rent: both paydays, oldest first.
	allocs, err := st.AllocationsForBucket(ctx, sc, rent, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 2 || allocs[0].Amount != 60000 || allocs[1].Amount != 40000 {
		t.Errorf("rent allocations = %+v, want 60000 then 40000", allocs)
	}
}
