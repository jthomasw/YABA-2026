package store_test

// Tests that "this month" and "today" are decided in the configured clock
// location, not the server's.

import (
	"context"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

func TestMonthlySeriesUsesTheConfiguredClockLocation(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}

	// 01:00 UTC on 1 October is 18:00 on 30 September in Los Angeles. A UTC
	// server used to put this household in October already.
	restore := store.SetClockForTest(time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC), la)
	defer restore()

	if got := store.Today(); got != "2026-09-30" {
		t.Errorf("Today() = %s, want 2026-09-30", got)
	}

	st, sc := newTestStore(t)
	addExpense(t, st, sc, 4200, "Groceries", "2026-09-15", true)

	series, err := st.MonthlySeries(context.Background(), sc, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("%d points, want 2", len(series))
	}
	if last := series[1]; last.Month != "2026-09" || last.Expense != 4200 {
		t.Errorf("last point = %+v, want September with its $42.00", last)
	}
	if series[0].Month != "2026-08" {
		t.Errorf("first point = %s, want 2026-08", series[0].Month)
	}
}
