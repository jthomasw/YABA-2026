package store_test

// Tests that budgets count spending by the same rule as the category chart:
// line-item categories when a transaction has lines, its label when it does
// not. They used to match the label only, so a Costco shop split into Food
// lines showed under Food in the chart and never touched the Food budget.

import (
	"context"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

func itemisedExpense(t *testing.T, st *store.Store, sc store.Scope, label, date string, items ...store.NewLineItem) int64 {
	t.Helper()
	var total money.Cents
	for _, it := range items {
		total += it.Amount
	}
	id := addExpense(t, st, sc, total, label, date, true)
	if err := st.SetLineItems(context.Background(), sc, id, items); err != nil {
		t.Fatalf("set line items: %v", err)
	}
	return id
}

func TestBudgetsAndBreakdownCountLineItemsTheSameWay(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	// $100 at Costco: $60 of food, $25 of household goods, and a $15 line
	// nobody categorised, which stays Costco spending.
	itemisedExpense(t, st, sc, "Costco", "2026-08-03",
		store.NewLineItem{Description: "Bananas", Category: "Food", Amount: 6000},
		store.NewLineItem{Description: "Bin bags", Category: "Household", Amount: 2500},
		store.NewLineItem{Description: "???", Category: "  ", Amount: 1500},
	)
	// Unsplit, and typed in lower case: still the Food budget.
	addExpense(t, st, sc, 1000, "food", "2026-08-10", true)
	// Unsplit with no label at all.
	addExpense(t, st, sc, 700, "  ", "2026-08-11", true)
	// Another month: counted in neither.
	itemisedExpense(t, st, sc, "Costco", "2026-07-30",
		store.NewLineItem{Description: "Cheese", Category: "Food", Amount: 9900})
	// Another household's Food: counted in neither.
	bob := newSecondUser(t, st, "bob@example.com")
	itemisedExpense(t, st, bob, "Costco", "2026-08-03",
		store.NewLineItem{Description: "Steak", Category: "Food", Amount: 5000})

	for _, b := range []struct {
		cat   string
		limit money.Cents
	}{{"Food", 20000}, {"Household", 5000}, {"Costco", 5000}, {store.Uncategorised, 5000}, {"Books", 5000}} {
		if err := st.SetBudget(ctx, sc, b.cat, b.limit); err != nil {
			t.Fatalf("set budget %s: %v", b.cat, err)
		}
	}

	budgets, err := st.ListBudgets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	spent := map[string]money.Cents{}
	for _, b := range budgets {
		spent[b.Category] = b.Spent
	}
	wantSpent := map[string]money.Cents{
		"Food": 7000, "Household": 2500, "Costco": 1500, store.Uncategorised: 700, "Books": 0,
	}
	for cat, want := range wantSpent {
		if spent[cat] != want {
			t.Errorf("%s budget spent = %s, want %s", cat, spent[cat].Display(), want.Display())
		}
	}

	breakdown, err := st.CategoryBreakdown(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]money.Cents{}
	var sum money.Cents
	for _, lt := range breakdown {
		got[lt.Label] = lt.Total
		sum += lt.Total
	}
	wantBreakdown := map[string]money.Cents{
		"Food": 6000, "food": 1000, "Household": 2500, "Costco": 1500, store.Uncategorised: 700,
	}
	if len(got) != len(wantBreakdown) {
		t.Errorf("breakdown = %+v, want %v", breakdown, wantBreakdown)
	}
	for label, want := range wantBreakdown {
		if got[label] != want {
			t.Errorf("breakdown %s = %s, want %s", label, got[label].Display(), want.Display())
		}
	}
	// Every cent of the month counted exactly once, split or not.
	if sum != 11700 {
		t.Errorf("breakdown sums to %s, want $117.00", sum.Display())
	}
}
