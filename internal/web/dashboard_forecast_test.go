package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestChartPeriodFollowsThePeriodMenu: a month covers that month; All time
// covers everything and ends with the current month.
func TestChartPeriodFollowsThePeriodMenu(t *testing.T) {
	cases := []struct {
		month, today string
		want         chartPeriod
	}{
		{"2026-10", "2026-10-29", chartPeriod{From: "2026-10-01", To: "2026-10-31"}},
		{"2026-09", "2026-10-29", chartPeriod{From: "2026-09-01", To: "2026-09-30"}},
		{"2028-02", "2026-10-29", chartPeriod{From: "2028-02-01", To: "2028-02-29"}},
		{"", "2026-10-29", chartPeriod{To: "2026-10-31"}},
		{"", "2026-12-15", chartPeriod{To: "2026-12-31"}},
	}
	for _, c := range cases {
		if got := chartPeriodFor(c.month, c.today); got != c.want {
			t.Errorf("chartPeriodFor(%q, %q) = %+v, want %+v", c.month, c.today, got, c.want)
		}
	}
}

// TestPeriodBalanceTrimsToTheMonth checks the history is cut to the period,
// starts on the 1st with the carried-over balance, and closes a past month on
// its last day.
func TestPeriodBalanceTrimsToTheMonth(t *testing.T) {
	history := []store.Point{
		{Date: "2026-08-28", Balance: 34000},
		{Date: "2026-09-15", Balance: 24000},
		{Date: "2026-10-27", Balance: 4000},
		{Date: "2026-10-29", Balance: 4500},
		{Date: "2026-11-03", Balance: 9000}, // future-dated: never drawn as history
	}
	dates := func(ps []store.Point) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Date+"="+p.Balance.Display())
		}
		return strings.Join(s, " ")
	}

	// The current month: opens on Oct 1 with September's closing balance,
	// runs to today, and leaves the rest of the month to the forecast.
	got := periodBalance(history, chartPeriodFor("2026-10", "2026-10-29"), "2026-10-29")
	if want := "2026-10-01=$240.00 2026-10-27=$40.00 2026-10-29=$45.00"; dates(got) != want {
		t.Errorf("October:\n got %s\nwant %s", dates(got), want)
	}

	// A past month is drawn 1st to last day.
	got = periodBalance(history, chartPeriodFor("2026-09", "2026-10-29"), "2026-10-29")
	if want := "2026-09-01=$340.00 2026-09-15=$240.00 2026-09-30=$240.00"; dates(got) != want {
		t.Errorf("September:\n got %s\nwant %s", dates(got), want)
	}

	// A month with no entries is a flat line at the carried-over balance.
	got = periodBalance(history, chartPeriodFor("2026-07", "2026-10-29"), "2026-10-29")
	if want := "2026-07-01=$0.00 2026-07-31=$0.00"; dates(got) != want {
		t.Errorf("July:\n got %s\nwant %s", dates(got), want)
	}

	// All time keeps every past point and drops only the future one.
	got = periodBalance(history, chartPeriodFor("", "2026-10-29"), "2026-10-29")
	if len(got) != 4 {
		t.Errorf("All time kept %d points, want 4: %s", len(got), dates(got))
	}

	if got := periodBalance(nil, chartPeriodFor("2026-10", "2026-10-29"), "2026-10-29"); len(got) != 0 {
		t.Error("an account with no history should stay empty")
	}
}

// TestForecastStopsAtThePeriodEnd checks one predicted point per day from
// tomorrow to the period's last day, and none at all once the period is over.
func TestForecastStopsAtThePeriodEnd(t *testing.T) {
	today, _ := time.Parse(store.DateLayout, store.Today())
	v := dashboardView{Cash: 10000}
	day := func(offset int) string { return today.AddDate(0, 0, offset).Format(store.DateLayout) }
	history := func() ([]string, []*float64) {
		x := 100.0
		return []string{day(-3)}, []*float64{&x}
	}

	labels, actual := history()
	labels, actual, forecast := forecastBalance(v, labels, actual, day(3))
	// day(-3) history, today's anchor, then 3 predicted days.
	if len(labels) != 5 || len(forecast) != 5 || len(actual) != 5 {
		t.Fatalf("got %d labels, %d forecast points, want 5", len(labels), len(forecast))
	}
	if labels[len(labels)-1] != day(3) {
		t.Errorf("forecast ends on %s, want %s", labels[len(labels)-1], day(3))
	}
	if forecast[0] != nil || forecast[1] == nil {
		t.Error("the forecast should start at today's balance and not before")
	}

	for _, end := range []string{day(0), day(-1), "not a date"} {
		labels, actual := history()
		_, _, forecast := forecastBalance(v, labels, actual, end)
		if len(forecast) != 0 {
			t.Errorf("end %q: got %d forecast points, want none", end, len(forecast))
		}
	}
}

// TestDashboardChartFollowsThePeriod drives the page: the current month
// predicts to its last day, and a past month shows no prediction.
func TestDashboardChartFollowsThePeriod(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	today, _ := time.Parse(store.DateLayout, store.Today())
	lastMonth := today.AddDate(0, -1, -today.Day()+1)
	for _, d := range []time.Time{lastMonth.AddDate(0, 0, 4), today} {
		if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{
			Kind: store.KindIncome, Label: "Salary", Amount: 50000,
			OccurredOn: d.Format(store.DateLayout),
		}); err != nil {
			t.Fatal(err)
		}
	}

	endOfMonth := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	label := "Predicted balance (to " + endOfMonth.Format("Jan 2") + ")"

	body := rig.do("GET", "/dashboard?month="+today.Format(store.MonthLayout), nil).Body.String()
	if endOfMonth.After(today) && !strings.Contains(body, label) {
		t.Errorf("the current month should predict to %s", endOfMonth.Format("Jan 2"))
	}
	if strings.Contains(body, lastMonth.Format(store.DateLayout)) {
		t.Error("the current month's chart still shows last month's history")
	}

	body = rig.do("GET", "/dashboard?month="+lastMonth.Format(store.MonthLayout), nil).Body.String()
	if !strings.Contains(body, `data-forecast="[]"`) {
		t.Error("a past month should show no prediction")
	}

	body = rig.do("GET", "/dashboard", nil).Body.String()
	if endOfMonth.After(today) && !strings.Contains(body, label) {
		t.Error("All time should predict to the end of the current month")
	}
}
