package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Per-unit ceilings on a repeat interval.
//
// These are not taste. frequencyN arrives from a form field, is stored as an
// INTEGER whose only constraint is "> 0", and is then multiplied: "every
// 4611686018427387904 weeks" overflows frequencyN*7 back to a multiple of a
// year and AddDate returns the SAME date, so the catch-up loop below spins
// forever holding the process's single database connection -- one POST, and
// every user's next request blocks. "Every 2000000000 months" is the other
// half: it produces "166668693-05-17", which is written to a transaction and
// then cannot be re-parsed, so the schedule can never advance and the page
// 500s from then on.
//
// The ceilings are set where a real schedule stops and nonsense begins: a year
// of days, a year of weeks, ten years of months.
const (
	maxFrequencyDays   = 365
	maxFrequencyWeeks  = 52
	maxFrequencyMonths = 120
)

// maxCatchUpOccurrences bounds one call to a Process function.
//
// A schedule starting in 1970 that repeats daily owes twenty thousand
// transactions; generating them one database transaction at a time on a
// single-connection pool would hang the request that triggered it. Stopping
// short is safe: the remainder is generated on the next visit, because
// next_due_date is advanced only as far as the work actually done.
const maxCatchUpOccurrences = 500

// ValidFrequency reports whether a repeat interval is one the schedules can
// actually carry, returning a message fit to show the user if it is not.
//
// Exported because the handlers validate the same form field before they get
// here, and two copies of these numbers would eventually disagree.
func ValidFrequency(frequencyN int, frequencyUnit string) (bool, string) {
	if frequencyN <= 0 {
		return false, "Repeat every must be at least 1."
	}

	max := 0
	switch frequencyUnit {
	case "day":
		max = maxFrequencyDays
	case "week":
		max = maxFrequencyWeeks
	case "month":
		max = maxFrequencyMonths
	default:
		return false, "Choose days, weeks or months."
	}

	if frequencyN > max {
		return false, fmt.Sprintf("Repeat every cannot be more than %d %ss.", max, frequencyUnit)
	}
	return true, ""
}

// advanceRecurringDate returns the occurrence after date for a schedule whose
// first occurrence was anchor (its start_date).
//
// Days and weeks step from date itself: a fixed number of days has no edge
// cases. Months do not, and this is why anchor is a parameter. Stepping from the
// previous occurrence with AddDate(0, n, 0) lets Go normalise an impossible
// date forward -- Jan 31 + 1 month is "Feb 31", which becomes Mar 3 -- and from
// then on the schedule runs on the 3rd forever and February is never charged.
// Instead the day of the month always comes from the anchor and is clamped to
// the last day of the target month, so Jan 31 monthly is Feb 28 (29 in a leap
// year), Mar 31, Apr 30, and never loses its place.
//
// The target month is date's month plus n rather than anchor + k*n counted from
// the start. For an untouched schedule the two are the same thing, because
// clamping never moves an occurrence out of its month. They differ only after an
// edit changes the interval, which the edit forms allow without moving the next
// due date: "every 2 months" then means two months after the occurrence already
// due, not whatever lands on a grid laid out from the start under the new
// interval, which could be a single month away.
func advanceRecurringDate(
	anchor string,
	date string,
	frequencyN int,
	frequencyUnit string,
) (string, error) {
	if ok, msg := ValidFrequency(frequencyN, frequencyUnit); !ok {
		return "", fmt.Errorf("invalid recurring frequency: %s", msg)
	}

	t, err := time.Parse(DateLayout, date)
	if err != nil {
		return "", fmt.Errorf("invalid recurring date: %w", err)
	}

	switch frequencyUnit {
	case "day":
		t = t.AddDate(0, 0, frequencyN)
	case "week":
		t = t.AddDate(0, 0, frequencyN*7)
	case "month":
		a, err := time.Parse(DateLayout, anchor)
		if err != nil {
			return "", fmt.Errorf("invalid recurring start date: %w", err)
		}
		t = addMonthsClamped(t, frequencyN, a.Day())
	default:
		return "", fmt.Errorf("invalid frequency unit %q", frequencyUnit)
	}

	next := t.Format(DateLayout)

	// The loops that call this terminate because the date goes up. A row stored
	// before the ceilings above existed could still hold a value that makes it
	// stand still, so that is an error here rather than an infinite loop there.
	if next <= date {
		return "", fmt.Errorf("recurring date did not advance past %s", date)
	}
	if _, err := time.Parse(DateLayout, next); err != nil {
		return "", fmt.Errorf("recurring date %q is out of range", next)
	}

	return next, nil
}

// addMonthsClamped returns day `day` of the month `months` after t's month,
// or that month's last day when it is shorter. The month arithmetic is done on
// the first of the month, where AddDate cannot overflow into the next one.
func addMonthsClamped(t time.Time, months, day int) time.Time {
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, months, 0)
	return time.Date(first.Year(), first.Month(), clampDay(first, day), 0, 0, 0, 0, time.UTC)
}

// clampDay returns day, or the last day of first's month when that is smaller.
// first must be the first of a month.
func clampDay(first time.Time, day int) int {
	last := first.AddDate(0, 1, -1).Day()
	if day > last {
		return last
	}
	return day
}

// ProcessAllDueRecurring catches up every household that has a recurring
// income or expense due by today. It is what the background runner calls, so
// scheduled money appears on the dashboard, in reports and in the funding
// waterfall without anybody first having to open Add Income or Add Expense.
//
// Each household is processed on its own; a failure in one is collected and
// the rest still run. It returns how many households were processed.
func (s *Store) ProcessAllDueRecurring(ctx context.Context) (int, error) {
	today := Today()

	// The acting user for the reallocation is one of the schedules' own
	// creators: allocations.user_id must name a real account.
	rows, err := s.db.QueryContext(ctx, `
		SELECT household_id, MIN(user_id) FROM (
			SELECT household_id, user_id FROM recurring_income
			WHERE active = 1 AND next_due_date <= ?
			UNION ALL
			SELECT household_id, user_id FROM recurring_expense
			WHERE active = 1 AND next_due_date <= ?
		)
		GROUP BY household_id`, today, today)
	if err != nil {
		return 0, fmt.Errorf("find due recurring schedules: %w", err)
	}
	var due []Scope
	for rows.Next() {
		var sc Scope
		if err := rows.Scan(&sc.HouseholdID, &sc.UserID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due household: %w", err)
		}
		due = append(due, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var errs []error
	for _, sc := range due {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := s.ProcessDueRecurringIncome(ctx, sc, today); err != nil {
			errs = append(errs, fmt.Errorf("household %d income: %w", sc.HouseholdID, err))
		}
		if err := s.ProcessDueRecurringExpenses(ctx, sc, today); err != nil {
			errs = append(errs, fmt.Errorf("household %d expenses: %w", sc.HouseholdID, err))
		}
	}
	return len(due), errors.Join(errs...)
}
