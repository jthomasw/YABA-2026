package store_test

// Tests for the month arithmetic behind the dashboard's charts and estimates.
//
// The bug these were written for: stepping back a month at a time from
// time.Now() with AddDate lands in the wrong month whenever today's day number
// does not exist in the target month, because AddDate normalises the overflow
// forwards. On the 31st, "three months ago" from May is 31 February, which is
// 3 March. The series then repeats some months and drops others, and everything
// derived from it -- the month-by-month bars, the income estimate, its
// confidence range -- is wrong for the last three days of most months.

import (
	"context"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestMonthsBackIsAlwaysDistinctAndConsecutive walks every day of a leap year
// and a common year, which covers every combination of a long month preceding a
// short one.
func TestMonthsBackIsAlwaysDistinctAndConsecutive(t *testing.T) {
	for _, year := range []int{2024, 2026} { // one leap, one not
		day := time.Date(year, 1, 1, 12, 0, 0, 0, time.UTC)
		for ; day.Year() == year; day = day.AddDate(0, 0, 1) {
			for _, n := range []int{2, 6, 12, 13} {
				got := store.MonthsBack(day, n)

				if len(got) != n {
					t.Fatalf("%s, n=%d: %d months", day.Format("2006-01-02"), n, len(got))
				}
				seen := map[string]bool{}
				for _, m := range got {
					if seen[m] {
						t.Fatalf("%s, n=%d: %q appears twice in %v",
							day.Format("2006-01-02"), n, m, got)
					}
					seen[m] = true
				}
				// The last entry is the month being viewed, and each step back
				// is exactly one calendar month.
				if want := day.Format("2006-01"); got[n-1] != want {
					t.Fatalf("%s, n=%d: series ends at %q, want %q",
						day.Format("2006-01-02"), n, got[n-1], want)
				}
				for i := 1; i < n; i++ {
					prev, _ := time.Parse("2006-01", got[i-1])
					if want := prev.AddDate(0, 1, 0).Format("2006-01"); got[i] != want {
						t.Fatalf("%s, n=%d: %q follows %q, want %q (%v)",
							day.Format("2006-01-02"), n, got[i], got[i-1], want, got)
					}
				}
			}
		}
	}
}

// TestMonthlySeriesOnTheThirtyFirst is the same bug seen through the query that
// feeds the chart: a month with money in it must appear exactly once, whatever
// today's date happens to be.
func TestMonthlySeriesOnTheThirtyFirst(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	// One distinctive amount per month, so a duplicated or dropped month is
	// visible in the totals rather than only in the labels.
	amounts := map[string]money.Cents{
		"2026-01": 100000, "2026-02": 200000, "2026-03": 300000,
		"2026-04": 400000, "2026-05": 500000,
	}
	for month, amt := range amounts {
		addIncome(t, st, sc, amt, "Pay", month+"-05")
	}

	series, err := st.MonthlySeriesAsOf(ctx, sc, 5, time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 5 {
		t.Fatalf("%d points, want 5", len(series))
	}

	got := map[string]money.Cents{}
	for _, p := range series {
		if _, dup := got[p.Month]; dup {
			t.Errorf("%s appears twice in the series", p.Month)
		}
		got[p.Month] = p.Income
	}
	for month, want := range amounts {
		if got[month] != want {
			t.Errorf("%s income = %d, want %d (series %+v)", month, got[month], want, series)
		}
	}
}
