package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestBillShowsNextDueDate checks a monthly bill with a due day shows when it
// is next due, on both pages the recurring section is drawn, and that the
// add and edit forms save the day.
func TestBillShowsNextDueDate(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.post("/buckets", url.Values{
		"name": {"Rent"}, "cost_kind": {"fixed"}, "amount": {"1000"}, "due_day": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add bill: %d", rec.Code)
	}
	bills, err := rig.store.Buckets(context.Background(), rig.scope, "")
	if err != nil || len(bills) != 1 || bills[0].DueDay != 1 {
		t.Fatalf("bill not saved with due day 1: %+v, %v", bills, err)
	}
	want := "Next due " + dateNameOf(t, bills[0].NextDueDate())

	for _, page := range []string{"/dashboard", "/expense?step=manual"} {
		body := rig.do("GET", page, nil).Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("%s does not show %q", page, want)
		}
		if !strings.Contains(body, fmt.Sprintf(`id="bdue-%d"`, bills[0].ID)) {
			t.Errorf("%s has no due-day field in the bill's edit form", page)
		}
	}

	// Editing moves the date.
	rig.post(fmt.Sprintf("/buckets/%d/edit", bills[0].ID), url.Values{
		"name": {"Rent"}, "cost_kind": {"fixed"}, "amount": {"1000"}, "due_day": {"20"},
	})
	bills, _ = rig.store.Buckets(context.Background(), rig.scope, "")
	if bills[0].DueDay != 20 {
		t.Errorf("edit did not change the due day: %d", bills[0].DueDay)
	}

	// A day that is not a day of the month is refused with a message.
	rec = rig.post("/buckets", url.Values{
		"name": {"Phone"}, "cost_kind": {"fixed"}, "amount": {"50"}, "due_day": {"40"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invalid due day: %d", rec.Code)
	}
	if bills, _ = rig.store.Buckets(context.Background(), rig.scope, ""); len(bills) != 1 {
		t.Error("a bill with due day 40 was saved")
	}
}

// TestBillWithoutDayUsesItsAutomaticExpense checks a bill with no due day of
// its own takes the date of the automatic expense that pays towards it, and
// that one with neither invites the user to add a day.
func TestBillWithoutDayUsesItsAutomaticExpense(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	car, err := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{
		Name: "Car payment", CostKind: store.CostFixed, Fixed: 50000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.CreateRecurringExpense(ctx, rig.scope,
		"Car loan", 50000, &car, false, 1, "month", "2099-03-15"); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{
		Name: "Groceries", CostKind: store.CostFixed, Fixed: 40000,
	}); err != nil {
		t.Fatal(err)
	}

	body := rig.do("GET", "/expense?step=manual", nil).Body.String()
	if !strings.Contains(body, "Next due 15 Mar 2099") {
		t.Error("the car payment does not take its automatic expense's date")
	}
	if !strings.Contains(body, "No due date yet") {
		t.Error("a bill with no date does not invite the user to add one")
	}
}

// TestTickerListsBillsWithADueDay checks the dashboard strip includes a bill
// with a due day, and does not list a bill twice when an automatic expense
// already pays it.
func TestTickerListsBillsWithADueDay(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	if _, err := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000, DueDay: 1,
	}); err != nil {
		t.Fatal(err)
	}
	gym, err := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{
		Name: "Gym", CostKind: store.CostFixed, Fixed: 3500, DueDay: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.CreateRecurringExpense(ctx, rig.scope,
		"Gym membership", 3500, &gym, false, 1, "month", "2099-01-05"); err != nil {
		t.Fatal(err)
	}

	body := rig.do("GET", "/dashboard", nil).Body.String()
	strip := body[strings.Index(body, `class="financial-ticker"`):]
	strip = strip[:strings.Index(strip, "</section>")]
	if !strings.Contains(strip, "Rent -$1,000.00") {
		t.Error("the ticker does not list the rent bill")
	}
	if strings.Count(strip, "$35.00") != 1 {
		t.Errorf("the gym payment should appear exactly once, found %d", strings.Count(strip, "$35.00"))
	}
}

// dateNameOf formats a YYYY-MM-DD the way the dateName template helper does.
func dateNameOf(t *testing.T, d string) string {
	t.Helper()
	parsed, err := time.Parse(store.DateLayout, d)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Format("2 Jan 2006")
}
