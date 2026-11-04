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

// A double click on Add (deposit) moves the money once, not twice.
func TestFundDepositDoubleClickMovesMoneyOnce(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{
		Kind: store.KindIncome, Label: "Salary", Amount: 200000, OccurredOn: store.Today()}); err != nil {
		t.Fatal(err)
	}
	fund, err := rig.store.CreateFund(ctx, rig.scope, "Holiday", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	page := rig.do("GET", "/dashboard?tab=current", nil).Body.String()
	form := url.Values{
		"form_token": {hiddenValue(t, page, "form_token")},
		"back":       {"/dashboard?tab=current"}, "amount": {"500.00"},
	}
	target := fmt.Sprintf("/funds/%d/deposit", fund)
	rig.post(target, form)
	rig.post(target, form) // the second click

	var n int
	var total int64
	rig.db.QueryRow(`SELECT COUNT(*), IFNULL(SUM(amount_cents),0) FROM transactions WHERE fund_id = ?`, fund).Scan(&n, &total)
	if n != 1 || total != 50000 {
		t.Fatalf("double click recorded %d transfer(s) totalling %d cents, want 1 of 50000", n, total)
	}

	// The next deposit, from a freshly loaded page, still goes through.
	page = rig.do("GET", "/dashboard?tab=current", nil).Body.String()
	form.Set("form_token", hiddenValue(t, page, "form_token"))
	rig.post(target, form)
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE fund_id = ?`, fund).Scan(&n)
	if n != 2 {
		t.Errorf("a second, separate deposit was refused (%d transfers)", n)
	}
}

func TestTidyReceiptDates(t *testing.T) {
	for in, want := range map[string]string{
		"2026-09-12": "2026-09-12", "2026-9-12": "2026-09-12", "2026/9/2": "2026-09-02",
		"2026-02-30": "2026-02-30", "12/09": "12/09", "": "",
	} {
		if got := store.TidyDate(in); got != want {
			t.Errorf("TidyDate(%q) = %q, want %q", in, got, want)
		}
	}
	d := store.ReceiptDraft{Date: "2026-02-30"}
	d.CheckDate()
	if d.Date != "" || d.BadDate != "2026-02-30" {
		t.Errorf("CheckDate kept an impossible date: %+v", d)
	}
}

// A receipt whose date isn't a real date says so, leaves the date box empty,
// and refuses to save the expense as today's until a date is entered.
func TestUnreadableReceiptDateIsNotSavedAsToday(t *testing.T) {
	rig := newRig(t)
	rig.login()
	job := rig.draftReceipt("till.jpg", store.ReceiptDraft{Merchant: "Lidl", Date: "2026-02-30", Total: 900})

	page := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", job), nil).Body.String()
	if !strings.Contains(page, "isn&#39;t\n          a real date") && !strings.Contains(page, "a real date") {
		t.Errorf("form does not say the receipt date was unreadable")
	}
	if strings.Contains(page, `name="date" type="date" required value="`+store.Today()+`"`) {
		t.Errorf("date box was pre-filled with today")
	}

	form := url.Values{
		"form_token": {hiddenValue(t, page, "form_token")}, "kind": {"expense"},
		"receipt_job": {fmt.Sprint(job)}, "label": {"Groceries"}, "amount": {"9.00"}, "date": {""},
	}
	rec := rig.post("/transactions/new", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "couldn") {
		t.Fatalf("empty date with an unreadable receipt date: status %d: %s", rec.Code, extractErr(rec.Body.String()))
	}
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE label = 'Groceries'`).Scan(&n)
	if n != 0 {
		t.Fatalf("saved %d expense(s) dated today", n)
	}

	// With a date typed in, it saves.
	form.Set("form_token", hiddenValue(t, rec.Body.String(), "form_token"))
	form.Set("date", "2026-02-27")
	rig.post("/transactions/new", form)
	var on string
	rig.db.QueryRow(`SELECT occurred_on FROM transactions WHERE label = 'Groceries'`).Scan(&on)
	if on != "2026-02-27" {
		t.Errorf("saved on %q, want 2026-02-27", on)
	}
}

// Line-item rows left over from One-time don't stop a recurring expense.
func TestRecurringExpenseIgnoresHiddenItemRows(t *testing.T) {
	rig := newRig(t)
	rig.login()
	rec := rig.post("/expense", url.Values{
		"expense_type": {"recurring"}, "frequency_preset": {"monthly"},
		"label": {"Netflix"}, "amount": {"15.00"}, "date": {"2026-12-01"},
		"item_description": {"Milk"}, "item_category": {""}, "item_amount": {"3.00"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("recurring expense refused (%d) over rows the form hides", rec.Code)
	}
}

func extractErr(body string) string {
	i := strings.Index(body, `role="alert"`)
	if i < 0 {
		return ""
	}
	end := i + 300
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}
