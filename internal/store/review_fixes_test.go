package store_test

import (
	"context"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// T1: a backdated bucket expense in last month changes THIS month's variable
// estimate, but the handler only re-pours the expense's own month.
func TestFixStaleWaterfallAfterBackdatedBucketExpense(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	elec, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Electric", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	addIncome(t, st, sc, 100000, "Salary", "2026-11-01")
	if err := st.Reallocate(ctx, sc, "2026-11"); err != nil {
		t.Fatal(err)
	}
	// User enters last month's power bill, attributed to the bucket. The web
	// handler then calls ReallocateMonthOf(occurredOn) only.
	if _, err := st.Add(ctx, sc, store.NewTransaction{
		Kind: store.KindExpense, Label: "Power", Amount: 30000,
		OccurredOn: "2026-10-28", BucketID: &elec,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReallocateMonthOf(ctx, sc, "2026-10-28"); err != nil {
		t.Fatal(err)
	}
	bs, _ := st.Buckets(ctx, sc, "2026-11")
	sum, _ := st.AllocationsFor(ctx, sc, "2026-11", bs)
	t.Logf("Nov: due=%s alloc=%s income=%s unassigned=%s shortfall=%s",
		bs[0].Due.Display(), bs[0].Allocated.Display(), sum.Income.Display(), sum.Unassigned.Display(), sum.Shortfall.Display())
	if sum.Unassigned > 0 && sum.Shortfall > 0 {
		t.Errorf("waterfall stale: %s unassigned while %s shortfall", sum.Unassigned.Display(), sum.Shortfall.Display())
	}
}

// T2: chart vs budget case sensitivity.
func TestFixCategoryBreakdownCase(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	addExpense(t, st, sc, 6000, "Food", "2026-11-01", true)
	addExpense(t, st, sc, 4000, "food", "2026-11-01", true)
	if err := st.SetBudget(ctx, sc, "Food", 20000); err != nil {
		t.Fatal(err)
	}
	cb, _ := st.CategoryBreakdown(ctx, sc, "2026-11")
	b, _ := st.ListBudgets(ctx, sc, "2026-11")
	t.Logf("breakdown=%+v budgets=%+v", cb, b)
	if len(cb) != 1 {
		t.Errorf("chart splits one category into %d slices while budget counts %s", len(cb), b[0].Spent.Display())
	}
}

// T3: recurring expense keeps posting to an archived bucket.
func TestFixRecurringIntoArchivedBucket(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	rent, _ := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Rent", CostKind: store.CostFixed, Fixed: 100000})
	id, err := st.CreateRecurringExpense(ctx, sc, "Rent", 100000, &rent, true, 1, "month", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveBucket(ctx, sc, rent); err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessDueRecurringExpenses(ctx, sc, "2026-11-02"); err != nil {
		t.Fatal(err)
	}
	r, _ := st.RecurringExpenseByID(ctx, sc, id)
	txs, _ := st.All(ctx, sc, store.Filter{})
	for _, tx := range txs {
		if tx.BucketID != nil {
			t.Errorf("tx %s attributed to archived bucket %d (%q); schedule shows bucket name %q",
				tx.OccurredOn, *tx.BucketID, tx.BucketName, r.BucketName)
			// editing it with the same bucket is refused
			b := *tx.BucketID
			err := st.UpdateWithItems(ctx, sc, tx.ID, store.NewTransaction{
				Kind: tx.Kind, Label: tx.Label, Amount: tx.Amount, OccurredOn: tx.OccurredOn,
				BucketID: &b, Version: tx.Version,
			}, nil)
			t.Logf("edit with same bucket -> %v", err)
			break
		}
	}
}

// T4: Parse rejects what Display prints.
