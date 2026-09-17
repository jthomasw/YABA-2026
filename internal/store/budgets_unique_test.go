package store_test

// Regression tests for two defects in the budgets table that produced wrong
// money on screen rather than merely an inconvenience.

import (
	"context"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Budgets moved from per-user to per-household in migration 4, but the original
// inline UNIQUE(user_id, category) was an implicit index that no DROP INDEX
// could reach, so it stayed live underneath. Anybody in two households could
// budget a category in the first and was then permanently refused in the second
// -- with the raw SQLite constraint text shown to them.
func TestTheSameCategoryCanBeBudgetedInTwoHouseholds(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	second, err := st.CreateSharedHousehold(ctx, sc.UserID, "Second")
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	sc2 := store.Scope{HouseholdID: second, UserID: sc.UserID}

	if err := st.SetBudget(ctx, sc, "Food", 5000); err != nil {
		t.Fatalf("first household: %v", err)
	}
	if err := st.SetBudget(ctx, sc2, "Food", 7000); err != nil {
		t.Fatalf("second household refused the same category: %v", err)
	}

	one, err := st.ListBudgets(ctx, sc, "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := st.ListBudgets(ctx, sc2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Limit != 5000 {
		t.Errorf("first household has %d budgets, want one of $50.00", len(one))
	}
	if len(two) != 1 || two[0].Limit != 7000 {
		t.Errorf("second household has %d budgets, want one of $70.00", len(two))
	}
}

// Spending is matched to a budget case-insensitively, but the uniqueness that is
// supposed to stop duplicate categories used to compare bytes. "Food" and "food"
// were therefore two budget rows matching the same spending, and a single
// $100.00 expense was counted in full against both -- two over-budget warnings
// for one piece of spending.
func TestBudgetCategoriesAreUniqueRegardlessOfCase(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	if err := st.SetBudget(ctx, sc, "Food", 5000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBudget(ctx, sc, "food", 7000); err != nil {
		t.Fatalf("the lower-case spelling was refused outright: %v", err)
	}
	if err := st.SetBudget(ctx, sc, "  FOOD  ", 9000); err != nil {
		t.Fatalf("the padded upper-case spelling was refused outright: %v", err)
	}

	budgets, err := st.ListBudgets(ctx, sc, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(budgets) != 1 {
		var got []string
		for _, b := range budgets {
			got = append(got, b.Category)
		}
		t.Fatalf("%d budget rows for one category (%s); want 1",
			len(budgets), strings.Join(got, ", "))
	}
	if budgets[0].Limit != 9000 {
		t.Errorf("limit = %s, want the most recently set $90.00", budgets[0].Limit.Display())
	}
}

// The consequence the user actually saw: one expense counted twice.
func TestOneExpenseIsNotCountedAgainstTwoSpellingsOfACategory(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	essential := false

	if _, err := st.Add(ctx, sc, store.NewTransaction{
		Kind: store.KindExpense, Label: "Food", Amount: 10000,
		OccurredOn: store.Today(), Essential: &essential,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBudget(ctx, sc, "Food", 5000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBudget(ctx, sc, "food", 5000); err != nil {
		t.Fatal(err)
	}

	budgets, err := st.ListBudgets(ctx, sc, store.Today()[:7])
	if err != nil {
		t.Fatal(err)
	}

	var counted store.Cents
	for _, b := range budgets {
		counted += b.Spent
	}
	if counted != 10000 {
		t.Errorf("$100.00 of spending was counted as %s across %d budget rows",
			counted.Display(), len(budgets))
	}
}
