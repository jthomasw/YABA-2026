package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// An over-budget category reads "$20.00 over", not "-$20.00 over".
func TestReportsOverBudgetIsPositive(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	if err := rig.store.SetBudget(ctx, rig.scope, "Food", 10000); err != nil {
		t.Fatal(err)
	}
	ess := true
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Food", Amount: 12000, OccurredOn: store.Today(), Essential: &ess}); err != nil {
		t.Fatal(err)
	}
	body := rig.do("GET", "/reports?month="+store.Today()[:7], nil).Body.String()
	if strings.Contains(body, "-$20.00 over") || !strings.Contains(body, "$20.00 over") {
		t.Errorf("reports page doesn't say \"$20.00 over\"")
	}
}

// A discount typed "-$2.00" is accepted as a line item.
func TestDiscountWithDollarSignAfterMinus(t *testing.T) {
	rig := newRig(t)
	rig.login()
	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	rec := rig.post("/expense", url.Values{
		"form_token": {hiddenValue(t, page, "form_token")}, "expense_type": {"one_time"},
		"label": {"Groceries"}, "amount": {"8.00"}, "date": {store.Today()},
		"item_description": {"Milk", "Coupon"}, "item_category": {"Food", "Food"},
		"item_amount": {"10.00", "-$2.00"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("\"-$2.00\" discount refused: %d", rec.Code)
	}
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM line_items WHERE amount_cents = -200`).Scan(&n)
	if n != 1 {
		t.Errorf("discount line not stored")
	}
}

// Who, Where and Why on a recurring expense are kept, shown, and carried onto
// every expense the schedule creates.
func TestRecurringExpenseKeepsWhoWhereWhy(t *testing.T) {
	rig := newRig(t)
	rig.login()
	today := store.Today()
	rec := rig.post("/expense", url.Values{
		"expense_type": {"recurring"}, "frequency_preset": {"monthly"},
		"label": {"Rent"}, "amount": {"900.00"}, "date": {today},
		"payee": {"Landlord"}, "place": {"Main St"}, "note": {"Flat 4B"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("recurring expense: %d", rec.Code)
	}
	ctx := context.Background()
	list, err := rig.store.ListRecurringExpense(ctx, rig.scope)
	if err != nil || len(list) != 1 {
		t.Fatalf("schedules: %v %v", list, err)
	}
	if r := list[0]; r.Payee != "Landlord" || r.Place != "Main St" || r.Note != "Flat 4B" {
		t.Fatalf("schedule lost its details: %+v", r)
	}
	if err := rig.store.ProcessDueRecurringExpenses(ctx, rig.scope, today); err != nil {
		t.Fatal(err)
	}
	var payee, place, note string
	if err := rig.db.QueryRow(`SELECT payee, place, note FROM transactions WHERE label = 'Rent'`).Scan(&payee, &place, &note); err != nil {
		t.Fatal(err)
	}
	if payee != "Landlord" || place != "Main St" || note != "Flat 4B" {
		t.Errorf("created expense has %q %q %q", payee, place, note)
	}
	if body := rig.do("GET", "/expense?step=manual&planner=open", nil).Body.String(); !strings.Contains(body, "To Landlord") {
		t.Errorf("schedule list doesn't show who it's paid to")
	}
}

// A real database error is never shown to the user, even unwrapped.
func TestDatabaseErrorsAreNotShown(t *testing.T) {
	rig := newRig(t)
	_, err := rig.db.Exec(`SELECT * FROM no_such_table`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if isUserFacing(err) {
		t.Errorf("raw driver error %q counts as user-facing", err)
	}
	if isUserFacing(fmt.Errorf("x: %w", err)) {
		t.Errorf("wrapped driver error counts as user-facing")
	}
	if !isUserFacing(errors.New("Give the fund a name.")) {
		t.Errorf("the store's own message is no longer shown")
	}
}

// "$1,000" as a starting balance is read as $1,000, not silently dropped; and
// something that isn't an amount is refused with a message, not saved as 0.
func TestSetupReadsDollarsAndCommas(t *testing.T) {
	rig := newRig(t)
	rig.login()
	page := rig.do("GET", "/setup", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="starting_balance" type="text"`) {
		t.Errorf("starting balance should be a text box (status %d)", page.Code)
	}
	rec := rig.post("/setup", url.Values{"starting_balance": {"abc"}, "expense_count": {"0"}, "income_count": {"0"}})
	if rec.Code == http.StatusSeeOther {
		t.Errorf("\"abc\" starting balance was accepted")
	}
	rig.post("/setup", url.Values{"starting_balance": {"$1,000"}, "expense_count": {"0"}, "income_count": {"0"}})
	tot, _ := rig.store.Totals(context.Background(), rig.scope, "")
	if tot.Income != 100000 {
		t.Errorf("starting balance = %s, want $1,000.00", tot.Income.Display())
	}
}
