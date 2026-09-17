package store_test

// The invariant SUM(line_items) == transactions.amount_cents is enforced by the
// write, not by a constraint, so it survives only as long as the amount and the
// breakdown are written together. These tests are what stops them drifting apart
// again.

import (
	"context"
	"errors"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

func splitExpense(t *testing.T, st *store.Store, sc store.Scope) int64 {
	t.Helper()
	ctx := context.Background()
	essential := true

	id, err := st.Add(ctx, sc, store.NewTransaction{
		Kind: store.KindExpense, Label: "Shop", Amount: 10000,
		OccurredOn: "2026-01-10", Essential: &essential,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.SetLineItems(ctx, sc, id, []store.NewLineItem{
		{Description: "Groceries", Category: "Food", Amount: 6000},
		{Description: "Paperback", Category: "Books", Amount: 4000},
	}); err != nil {
		t.Fatalf("set items: %v", err)
	}
	return id
}

// itemSum is the breakdown's own total, which must always equal the
// transaction's amount.
func itemSum(t *testing.T, st *store.Store, sc store.Scope, id int64) money.Cents {
	t.Helper()
	items, err := st.LineItems(context.Background(), sc, id)
	if err != nil {
		t.Fatalf("line items: %v", err)
	}
	var sum money.Cents
	for _, it := range items {
		sum += it.Amount
	}
	return sum
}

// A rejected edit must leave BOTH halves untouched. Before UpdateWithItems, the
// amount was committed by its own transaction first, so a failure after that
// point left a $50.00 expense whose breakdown still said $100.00 -- the
// dashboard headline and its own category chart disagreeing by two times.
func TestRejectedEditLeavesAmountAndItemsConsistent(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := splitExpense(t, st, sc)

	before, err := st.ByID(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}

	// Halve the amount but leave the old breakdown: this cannot balance.
	err = st.UpdateWithItems(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Shop", Amount: 5000,
		OccurredOn: "2026-01-10", Version: before.Version,
	}, []store.NewLineItem{
		{Description: "Groceries", Category: "Food", Amount: 6000},
		{Description: "Paperback", Category: "Books", Amount: 4000},
	})
	if err == nil {
		t.Fatal("an unbalanced edit was accepted")
	}
	if !errors.Is(err, store.ErrItemsDoNotBalance) {
		t.Errorf("error is %v, want ErrItemsDoNotBalance so the handler can show a sentence", err)
	}

	after, err := st.ByID(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Amount != before.Amount {
		t.Errorf("amount changed to %s despite the failure; want %s",
			after.Amount.Display(), before.Amount.Display())
	}
	if got := itemSum(t, st, sc, id); got != after.Amount {
		t.Errorf("breakdown sums to %s but the transaction is %s", got.Display(), after.Amount.Display())
	}
	if after.Version != before.Version {
		t.Errorf("version moved from %d to %d on a failed save", before.Version, after.Version)
	}
}

// The happy path: a new amount with a matching new breakdown replaces both.
func TestEditReplacesAmountAndItemsTogether(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := splitExpense(t, st, sc)

	before, _ := st.ByID(ctx, sc, id)
	if err := st.UpdateWithItems(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Shop", Amount: 5000,
		OccurredOn: "2026-01-10", Version: before.Version,
	}, []store.NewLineItem{
		{Description: "Groceries", Category: "Food", Amount: 3000},
		{Description: "Paperback", Category: "Books", Amount: 2000},
	}); err != nil {
		t.Fatalf("balanced edit refused: %v", err)
	}

	after, _ := st.ByID(ctx, sc, id)
	if after.Amount != 5000 {
		t.Errorf("amount = %s, want $50.00", after.Amount.Display())
	}
	if got := itemSum(t, st, sc, id); got != 5000 {
		t.Errorf("breakdown sums to %s, want $50.00", got.Display())
	}
}

// Clearing the breakdown is how somebody removes it, and must still be possible
// now that the two writes share a transaction.
func TestEditCanClearTheBreakdown(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := splitExpense(t, st, sc)

	before, _ := st.ByID(ctx, sc, id)
	if err := st.UpdateWithItems(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Shop", Amount: 7500,
		OccurredOn: "2026-01-10", Version: before.Version,
	}, nil); err != nil {
		t.Fatalf("clearing refused: %v", err)
	}

	items, err := st.LineItems(ctx, sc, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("%d line items survived the clear", len(items))
	}
	after, _ := st.ByID(ctx, sc, id)
	if after.Amount != 7500 {
		t.Errorf("amount = %s, want $75.00", after.Amount.Display())
	}
}

// The staleness check still applies on the atomic path.
func TestAtomicEditStillRefusesAStaleVersion(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := splitExpense(t, st, sc)

	before, _ := st.ByID(ctx, sc, id)
	err := st.UpdateWithItems(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Shop", Amount: 5000,
		OccurredOn: "2026-01-10", Version: before.Version + 5,
	}, nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("error is %v, want ErrConflict", err)
	}

	after, _ := st.ByID(ctx, sc, id)
	if after.Amount != before.Amount {
		t.Error("a stale save changed the amount")
	}
	if got := itemSum(t, st, sc, id); got != after.Amount {
		t.Errorf("breakdown sums to %s but the transaction is %s", got.Display(), after.Amount.Display())
	}
}
