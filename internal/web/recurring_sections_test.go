package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// seedRecurring gives the test household one monthly bill, one automatic
// recurring expense and one recurring income, all starting in the future so
// opening a page charges nothing.
func seedRecurring(t *testing.T, rig *testRig) (bucketID, expenseID, incomeID int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	if bucketID, err = rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000, Essential: true,
	}); err != nil {
		t.Fatal(err)
	}
	if expenseID, err = rig.store.CreateRecurringExpense(ctx, rig.scope,
		"Gym", 3500, nil, false, 1, "month", "2099-01-01"); err != nil {
		t.Fatal(err)
	}
	if incomeID, err = rig.store.CreateRecurringIncome(ctx, rig.scope,
		"Salary", 200000, 2, "week", "2099-01-01"); err != nil {
		t.Fatal(err)
	}
	return bucketID, expenseID, incomeID
}

// TestRecurringExpensesShownOnDashboardAndManualOnly checks the one section is
// drawn on the dashboard's Expenses tab and under Add Expense -> Manual, with
// both the monthly bill and the automatic expense, and not on the chooser.
func TestRecurringExpensesShownOnDashboardAndManualOnly(t *testing.T) {
	rig := newRig(t)
	rig.login()
	bucketID, expenseID, _ := seedRecurring(t, rig)

	for page, returnTo := range map[string]string{
		"/dashboard":           returnDashboard,
		"/expense?step=manual": returnEntry,
	} {
		body := rig.do("GET", page, nil).Body.String()
		for _, want := range []string{
			`id="recurring-expenses"`,
			"Recurring expenses (2)",
			"Rent", "Gym",
			fmt.Sprintf(`action="/buckets/%d/edit"`, bucketID),
			fmt.Sprintf(`action="/expense/recurring/%d/edit"`, expenseID),
			fmt.Sprintf(`action="/expense/recurring/%d/cancel"`, expenseID),
			`action="/buckets"`,
			fmt.Sprintf(`name="return_to" value="%s"`, returnTo),
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s is missing %q", page, want)
			}
		}
	}

	chooser := rig.do("GET", "/expense", nil).Body.String()
	if strings.Contains(chooser, `id="recurring-expenses"`) || strings.Contains(chooser, "Gym") {
		t.Error("the Add Expense chooser still shows recurring expenses")
	}
}

// TestRecurringIncomeShownOnDashboardAndAddIncome checks the income section is
// drawn, editable, in both places.
func TestRecurringIncomeShownOnDashboardAndAddIncome(t *testing.T) {
	rig := newRig(t)
	rig.login()
	_, _, incomeID := seedRecurring(t, rig)

	for page, returnTo := range map[string]string{
		"/dashboard": returnDashboard,
		"/income":    returnEntry,
	} {
		body := rig.do("GET", page, nil).Body.String()
		for _, want := range []string{
			`id="recurring-income"`,
			"Recurring income (1)",
			"Salary",
			fmt.Sprintf(`action="/income/%d/edit"`, incomeID),
			fmt.Sprintf(`action="/income/%d/cancel"`, incomeID),
			fmt.Sprintf(`name="return_to" value="%s"`, returnTo),
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s is missing %q", page, want)
			}
		}
	}
}

// TestRecurringActionsReturnToTheirPage checks every action in the sections
// lands the user back where they pressed it, and that return_to only ever
// chooses between the two known pages.
func TestRecurringActionsReturnToTheirPage(t *testing.T) {
	rig := newRig(t)
	rig.login()
	bucketID, expenseID, incomeID := seedRecurring(t, rig)

	bucketEdit := url.Values{"name": {"Rent"}, "cost_kind": {"fixed"}, "amount": {"1000"}, "essential": {"yes"}}
	expenseEdit := url.Values{"label": {"Gym"}, "amount": {"35"}, "frequency_n": {"1"}, "frequency_unit": {"month"}}
	incomeEdit := url.Values{"label": {"Salary"}, "amount": {"2000"}, "frequency_n": {"2"}, "frequency_unit": {"week"}}

	cases := []struct {
		path     string
		form     url.Values
		returnTo string
		want     string
	}{
		{fmt.Sprintf("/buckets/%d/edit", bucketID), bucketEdit, returnEntry, expensesEntryAnchor},
		{fmt.Sprintf("/buckets/%d/edit", bucketID), bucketEdit, returnDashboard, expensesTab},
		{fmt.Sprintf("/buckets/%d/up", bucketID), url.Values{}, returnEntry, expensesEntryAnchor},
		{"/buckets/reallocate", url.Values{"month": {store.Today()[:7]}}, returnEntry, expensesEntryAnchor},
		{fmt.Sprintf("/expense/recurring/%d/edit", expenseID), expenseEdit, returnDashboard, expensesTab},
		{fmt.Sprintf("/expense/recurring/%d/edit", expenseID), expenseEdit, returnEntry, expensesEntryAnchor},
		{fmt.Sprintf("/income/%d/edit", incomeID), incomeEdit, returnDashboard, incomeTab},
		{fmt.Sprintf("/income/%d/edit", incomeID), incomeEdit, returnEntry, incomeEntryAnchor},
		// Anything else falls back to the dashboard rather than going anywhere
		// it names.
		{fmt.Sprintf("/income/%d/edit", incomeID), incomeEdit, "https://evil.example", incomeTab},
	}
	for _, c := range cases {
		form := url.Values{"return_to": {c.returnTo}}
		for k, v := range c.form {
			form[k] = v
		}
		rec := rig.post(c.path, form)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.want {
			t.Errorf("POST %s return_to=%q: %d -> %q, want %q",
				c.path, c.returnTo, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}

	// Cancelling last, since it ends each schedule.
	if rec := rig.post(fmt.Sprintf("/expense/recurring/%d/cancel", expenseID),
		url.Values{"return_to": {returnEntry}}); rec.Header().Get("Location") != expensesEntryAnchor {
		t.Errorf("cancelling a recurring expense from Add Expense went to %q", rec.Header().Get("Location"))
	}
	if rec := rig.post(fmt.Sprintf("/income/%d/cancel", incomeID),
		url.Values{"return_to": {returnDashboard}}); rec.Header().Get("Location") != incomeTab {
		t.Errorf("cancelling recurring income from the dashboard went to %q", rec.Header().Get("Location"))
	}
}

// TestSectionReopensAfterAction checks the ?..=open the redirects carry really
// renders the section expanded, so it does not snap shut after a save.
func TestSectionReopensAfterAction(t *testing.T) {
	rig := newRig(t)
	rig.login()
	seedRecurring(t, rig)

	for page, id := range map[string]string{
		"/expense?step=manual&planner=open":    "recurring-expenses",
		"/dashboard?tab=expenses&planner=open": "recurring-expenses",
		"/income?recurring=open":               "recurring-income",
		"/dashboard?tab=income&recurring=open": "recurring-income",
	} {
		body := rig.do("GET", page, nil).Body.String()
		if !strings.Contains(body, `id="`+id+`" open`) {
			t.Errorf("%s does not render #%s open", page, id)
		}
	}
	if body := rig.do("GET", "/income", nil).Body.String(); strings.Contains(body, `id="recurring-income" open`) {
		t.Error("the income section should start collapsed")
	}
}

// TestCreatingRecurringShowsItInTheList checks a recurring entry saved from
// the entry page returns to that page's open list, where it now appears.
func TestCreatingRecurringShowsItInTheList(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.post("/income", url.Values{
		"income_type": {"recurring"}, "label": {"Paycheck"}, "amount": {"1500"},
		"occurred_on": {"2099-01-01"}, "frequency_n": {"2"}, "frequency_unit": {"week"},
	})
	if got := rec.Header().Get("Location"); got != incomeEntryAnchor {
		t.Fatalf("saving recurring income went to %q, want %q", got, incomeEntryAnchor)
	}
	if !strings.Contains(rig.do("GET", "/income", nil).Body.String(), "Paycheck") {
		t.Error("the new recurring income is not listed on Add Income")
	}
}
