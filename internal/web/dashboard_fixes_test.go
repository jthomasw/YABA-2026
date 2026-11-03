package web

import (
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

func rvToday(t *testing.T) time.Time {
	d, err := time.Parse(store.DateLayout, store.Today())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func rvEndOfMonth(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC)
}

// A: the daily spending baseline is the CURRENT (partial) month's spend / 30.
func TestDashboardFixPartialMonthBaseline(t *testing.T) {
	today := rvToday(t)
	end := rvEndOfMonth(today)
	if !end.After(today) {
		t.Skip("last day of month")
	}
	lastMonth := today.AddDate(0, 0, -today.Day()).Format(store.MonthLayout)
	v := dashboardView{
		Cash: 500000,
		Monthly: []store.MonthPoint{
			{Month: lastMonth, Expense: 300000},                     // $3,000 last month, ~$100/day
			{Month: today.Format(store.MonthLayout), Expense: 5000}, // $50 so far this month
		},
	}
	x := 5000.0
	_, _, f := forecastBalance(v, []string{today.Format(store.DateLayout)}, []*float64{&x}, end.Format(store.DateLayout))
	days := end.Sub(today).Hours() / 24
	got := *f[len(f)-1]
	t.Logf("today=%s end=%s days=%.0f predicted end balance=$%.2f (at last month's pace ~$%.2f)",
		today.Format(store.DateLayout), end.Format(store.DateLayout), days, got, 5000-100*days)
	if got > 4900 {
		t.Errorf("BUG: forecast assumes only $%.2f/day of spending because the baseline is the partial current month", (5000-got)/days)
	}
}

// A2: rent paid on the 1st as a bill with due day 1 -> partial month spend minus full recurring = 0.
func TestDashboardFixPartialMonthBaselineZeroedByRecurring(t *testing.T) {
	today := rvToday(t)
	end := rvEndOfMonth(today)
	if !end.After(today) {
		t.Skip()
	}
	lastMonth := today.AddDate(0, 0, -today.Day()).Format(store.MonthLayout)
	v := dashboardView{
		Cash: 500000,
		Monthly: []store.MonthPoint{
			{Month: lastMonth, Expense: 400000},                       // $1,200 rent + $2,800 other
			{Month: today.Format(store.MonthLayout), Expense: 130000}, // rent + $100 groceries
		},
		Buckets: []store.Bucket{{ID: 1, Name: "Rent", DueDay: 1, Due: 120000, Spent: 120000, CostKind: store.CostFixed, Fixed: 120000}},
	}
	x := 3700.0
	_, _, f := forecastBalance(v, []string{today.Format(store.DateLayout)}, []*float64{&x}, end.Format(store.DateLayout))
	t.Logf("predicted end balance=$%.2f, cash=$5000", *f[len(f)-1])
}

// B: a bill already paid this month (Spent >= Due) whose due day is still ahead is subtracted again.
func TestDashboardFixBillPaidEarlyCountedTwice(t *testing.T) {
	today := rvToday(t)
	end := rvEndOfMonth(today)
	due := today.AddDate(0, 0, 5)
	if due.After(end) {
		t.Skip()
	}
	v := dashboardView{
		Cash: 500000,
		Buckets: []store.Bucket{{
			ID: 7, Name: "Rent", DueDay: due.Day(), CostKind: store.CostFixed, Fixed: 100000,
			Due: 100000, Spent: 100000, // already paid in full this month
		}},
	}
	x := 5000.0
	labels, _, f := forecastBalance(v, []string{today.Format(store.DateLayout)}, []*float64{&x}, end.Format(store.DateLayout))
	for i, l := range labels {
		if l == due.Format(store.DateLayout) {
			t.Logf("forecast on %s = $%.2f (day before $%.2f)", l, *f[i], *f[i-1])
			if *f[i] < *f[i-1]-999 {
				t.Errorf("BUG: rent already paid (Spent == Due) is subtracted again on %s", l)
			}
		}
	}
}

// C: a schedule due today but not yet materialised (the runner is hourly; the tutorial
// path redirects to /dashboard which does not catch up) is dropped from the forecast.
func TestDashboardFixDueTodayNotYetProcessedVanishes(t *testing.T) {
	today := rvToday(t)
	end := rvEndOfMonth(today)
	if !end.After(today) {
		t.Skip()
	}
	td := today.Format(store.DateLayout)
	v := dashboardView{
		Cash: 0,
		RecurringIncome: []store.RecurringIncome{{
			ID: 1, Source: "Salary", Amount: 300000, FrequencyN: 1, FrequencyUnit: "month",
			StartDate: td, NextDueDate: td, Active: true,
		}},
	}
	x := 0.0
	_, _, f := forecastBalance(v, []string{td}, []*float64{&x}, end.Format(store.DateLayout))
	t.Logf("end-of-month forecast with $3000 salary due today (not yet recorded) = $%.2f", *f[len(f)-1])
	if *f[len(f)-1] < 1 {
		t.Errorf("BUG: unprocessed occurrence due today is in neither cash nor forecast")
	}
}

func rvFundID(t *testing.T, rig *testRig) int64 {
	ef, err := rig.store.EmergencyFund(context.Background(), rig.scope)
	if err != nil {
		t.Fatal(err)
	}
	return ef.ID
}

// D: the emergency fund's "Within (months)" horizon becomes the months-of-essentials multiplier.
func TestDashboardFixEssentialScheduleIgnoredByEmergency(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	if _, err := rig.store.CreateRecurringExpense(ctx, rig.scope, "Insurance", 50000, nil, true, 1, "month", rvToday(t).AddDate(0, 0, 3).Format(store.DateLayout)); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(rig.do("GET", "/dashboard?tab=emergency", nil).Body.String())
	if strings.Contains(body, "Nothing set aside yet, and no essential expenses marked") {
		t.Errorf("BUG: an essential $500/month recurring expense is not counted: emergency tab says no essential expenses are marked")
	}
}

// F: the emergency target follows the Period menu (variable essential bucket).
func TestDashboardFixEmergencyTargetFollowsPeriod(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	bid, err := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{Name: "Power", CostKind: store.CostVariable, Essential: true, DueDay: 28})
	if err != nil {
		t.Fatal(err)
	}
	today := rvToday(t)
	lm := today.AddDate(0, 0, -today.Day()) // last day of last month
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindIncome, Label: "Pay", Amount: 900000, OccurredOn: lm.AddDate(0, 0, -20).Format(store.DateLayout)}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindExpense, Label: "Power", Amount: 40000, OccurredOn: lm.Format(store.DateLayout), BucketID: &bid}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindExpense, Label: "Power", Amount: 10000, OccurredOn: lm.AddDate(0, 0, -40).Format(store.DateLayout), BucketID: &bid}); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`of a (\$[0-9,.]+) target`)
	tk := regexp.MustCompile(`Power -(\$[0-9,.]+)`)
	a := rig.do("GET", "/dashboard", nil).Body.String()
	b := rig.do("GET", "/dashboard?month="+lm.Format(store.MonthLayout), nil).Body.String()
	t.Logf("all time: target %v ticker %v", re.FindStringSubmatch(a), tk.FindStringSubmatch(a))
	t.Logf("last month: target %v ticker %v", re.FindStringSubmatch(b), tk.FindStringSubmatch(b))
}

// G: future-dated expense: chart draws two different balances for today; history line includes the future.
func TestDashboardFixFutureDatedInTodayAnchor(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	today := rvToday(t)
	end := rvEndOfMonth(today)
	fut := today.AddDate(0, 0, 5)
	if fut.After(end) {
		t.Skip()
	}
	rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindIncome, Label: "Pay", Amount: 100000, OccurredOn: today.Format(store.DateLayout)})
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindExpense, Label: "Car", Amount: 30000, OccurredOn: fut.Format(store.DateLayout)}); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(rig.do("GET", "/dashboard", nil).Body.String())
	vals := regexp.MustCompile(`data-values="([^"]*)"`).FindStringSubmatch(body)
	fc := regexp.MustCompile(`data-forecast="([^"]*)"`).FindStringSubmatch(body)
	lb := regexp.MustCompile(`data-labels="([^"]*)"`).FindStringSubmatch(body)
	t.Logf("labels %s", lb[1])
	t.Logf("values %s", vals[1])
	t.Logf("forecast %s", fc[1])

	// Today's balance is the same on both lines ($1,000: the $300 has not
	// happened), and the forecast ends $300 lower, not more.
	if !strings.HasPrefix(vals[1], "[1000") && !strings.Contains(vals[1], ",1000,") {
		t.Errorf("today's actual balance should be 1000: %s", vals[1])
	}
	if !strings.HasSuffix(fc[1], ",700]") {
		t.Errorf("forecast should end at 700 (1000 less the $300 on its date): %s", fc[1])
	}
}

// H: reports shows "-$X over" (double negative).
