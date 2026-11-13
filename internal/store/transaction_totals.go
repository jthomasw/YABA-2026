package store

import (
	"context"
	"fmt"
	"time"
)

// ── aggregates ────────────────────────────────────────────────────────────────

// Totals is the headline set of figures for a period.
type Totals struct {
	Income      Cents
	Expense     Cents
	Deposits    Cents
	Withdrawals Cents
}

// Cash is money available to spend right now: income received, less real
// spending, less whatever has been moved into savings.
func (t Totals) Cash() Cents {
	return t.Income - t.Expense - t.Deposits + t.Withdrawals
}

// Saved is the amount currently sitting in funds.
func (t Totals) Saved() Cents {
	return t.Deposits - t.Withdrawals
}

// NetWorth is cash plus savings. Transfers cancel out, so this figure is
// unaffected by moving money between pots.
func (t Totals) NetWorth() Cents {
	return t.Income - t.Expense
}

// Totals aggregates all four kinds in a single pass.
func (s *Store) Totals(ctx context.Context, sc Scope, month string) (Totals, error) {
	clause, span, err := monthClause(month, "")
	if err != nil {
		return Totals{}, err
	}

	q := `
		SELECT
			IFNULL(SUM(CASE kind WHEN 'income'          THEN amount_cents ELSE 0 END), 0),
			IFNULL(SUM(CASE kind WHEN 'expense'         THEN amount_cents ELSE 0 END), 0),
			IFNULL(SUM(CASE kind WHEN 'fund_deposit'    THEN amount_cents ELSE 0 END), 0),
			IFNULL(SUM(CASE kind WHEN 'fund_withdrawal' THEN amount_cents ELSE 0 END), 0)
		FROM transactions
		WHERE household_id = ?` + clause
	args := append([]any{sc.HouseholdID}, span...)

	var t Totals
	var in, ex, dep, wd int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&in, &ex, &dep, &wd); err != nil {
		return Totals{}, fmt.Errorf("totals: %w", err)
	}
	t.Income, t.Expense = Cents(in), Cents(ex)
	t.Deposits, t.Withdrawals = Cents(dep), Cents(wd)
	return t, nil
}

// Cash returns spendable cash across all time, which is the figure the
// deposit path must check before allowing a transfer.
func (s *Store) Cash(ctx context.Context, sc Scope) (Cents, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`SELECT IFNULL(SUM(`+cashSignSQL+`), 0) FROM transactions WHERE household_id = ?`,
		sc.HouseholdID,
	).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("cash balance: %w", err)
	}
	return Cents(v), nil
}

// Breakdown totals one kind by label, largest first, for a pie chart.
func (s *Store) Breakdown(ctx context.Context, sc Scope, kind Kind, month string) ([]LabelTotal, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("unknown transaction type %q", kind)
	}
	clause, span, err := monthClause(month, "")
	if err != nil {
		return nil, err
	}

	// Grouped without regard to case, like category budgets: "Salary" and
	// "salary" are one source, not two slices. MIN picks the capitalised
	// spelling to show, since upper case sorts first.
	q := `
		SELECT MIN(COALESCE(NULLIF(TRIM(label), ''), ?)) AS grp,
		       SUM(amount_cents)
		FROM transactions
		WHERE household_id = ? AND kind = ?` + clause
	args := append([]any{Uncategorised, sc.HouseholdID, string(kind)}, span...)
	q += ` GROUP BY LOWER(COALESCE(NULLIF(TRIM(label), ''), '')) ORDER BY SUM(amount_cents) DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("breakdown: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice: encoding/json renders nil as "null", which makes
	// Chart.js throw, whereas an empty slice renders as "[]" and draws nothing.
	out := []LabelTotal{}
	for rows.Next() {
		var lt LabelTotal
		var v int64
		if err := rows.Scan(&lt.Label, &v); err != nil {
			return nil, fmt.Errorf("scan breakdown: %w", err)
		}
		lt.Total = Cents(v)
		out = append(out, lt)
	}
	return out, rows.Err()
}

// EssentialSplit divides real spending into essential and non-essential.
func (s *Store) EssentialSplit(ctx context.Context, sc Scope, month string) (essential, other Cents, err error) {
	clause, span, err := monthClause(month, "")
	if err != nil {
		return 0, 0, err
	}
	q := `
		SELECT IFNULL(SUM(CASE WHEN essential = 1 THEN amount_cents ELSE 0 END), 0),
		       IFNULL(SUM(CASE WHEN essential = 0 THEN amount_cents ELSE 0 END), 0)
		FROM transactions
		WHERE household_id = ? AND kind = 'expense'` + clause
	args := append([]any{sc.HouseholdID}, span...)
	var e, o int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&e, &o); err != nil {
		return 0, 0, fmt.Errorf("essential split: %w", err)
	}
	return Cents(e), Cents(o), nil
}

// Point is one step on the running-balance line.
type Point struct {
	Date    string
	Balance Cents
}

// FutureCash is the cash effect of entries dated after `after` (YYYY-MM-DD):
// money recorded now for a day that has not come yet, such as next Friday's
// paycheck. It returns the net per date and the total. Cash counts these
// already; the dashboard takes them out of today's balance and adds each one
// to the forecast on its own day.
func (s *Store) FutureCash(ctx context.Context, sc Scope, after string) (map[string]Cents, Cents, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT occurred_on, SUM(`+cashSignSQL+`)
		FROM transactions
		WHERE household_id = ? AND occurred_on > ?
		GROUP BY occurred_on`, sc.HouseholdID, after)
	if err != nil {
		return nil, 0, fmt.Errorf("future cash: %w", err)
	}
	defer rows.Close()

	byDate := map[string]Cents{}
	var total Cents
	for rows.Next() {
		var date string
		var net int64
		if err := rows.Scan(&date, &net); err != nil {
			return nil, 0, fmt.Errorf("scan future cash: %w", err)
		}
		byDate[date] = Cents(net)
		total += Cents(net)
	}
	return byDate, total, rows.Err()
}

// BalanceSeries returns the running cash balance over time, accumulated in SQL with a
// window function so the query returns one row per day rather than one per
// transaction -- 90 points instead of 2,000.
func (s *Store) BalanceSeries(ctx context.Context, sc Scope) ([]Point, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT occurred_on,
		       SUM(daily) OVER (ORDER BY occurred_on) AS running
		FROM (
			SELECT occurred_on, SUM(`+cashSignSQL+`) AS daily
			FROM transactions
			WHERE household_id = ?
			GROUP BY occurred_on
		)
		ORDER BY occurred_on`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("balance series: %w", err)
	}
	defer rows.Close()

	out := []Point{}
	for rows.Next() {
		var p Point
		var v int64
		if err := rows.Scan(&p.Date, &v); err != nil {
			return nil, fmt.Errorf("scan balance series: %w", err)
		}
		p.Balance = Cents(v)
		out = append(out, p)
	}
	return out, rows.Err()
}

// MonthPoint is one bar in the month-over-month comparison.
type MonthPoint struct {
	Month   string // YYYY-MM
	Income  Cents
	Expense Cents
}

// Net is income less spending for the month; negative means overspending.
func (m MonthPoint) Net() Cents { return m.Income - m.Expense }

// SavingsRate is the share of income not spent, as a percentage.
func (m MonthPoint) SavingsRate() float64 {
	if m.Income <= 0 {
		return 0
	}
	return float64(m.Income-m.Expense) / float64(m.Income) * 100
}

// MonthlySeries returns the last n calendar months of income and spending,
// oldest first, including months with no activity so the chart has no gaps.
// "This month" is decided in the configured clock location (see Now), so a
// server running in UTC does not start a Pacific household's October at 5pm
// on 30 September.
func (s *Store) MonthlySeries(ctx context.Context, sc Scope, n int) ([]MonthPoint, error) {
	return s.MonthlySeriesAsOf(ctx, sc, n, Now())
}

// MonthlySeriesAsOf is MonthlySeries ending at a given date, so the month
// arithmetic can be tested on the days it used to get wrong.
func (s *Store) MonthlySeriesAsOf(ctx context.Context, sc Scope, n int, now time.Time) ([]MonthPoint, error) {
	if n <= 0 {
		n = 6
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(occurred_on, 1, 7) AS m,
		       IFNULL(SUM(CASE kind WHEN 'income'  THEN amount_cents ELSE 0 END), 0),
		       IFNULL(SUM(CASE kind WHEN 'expense' THEN amount_cents ELSE 0 END), 0)
		FROM transactions
		WHERE household_id = ?
		GROUP BY m`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("monthly series: %w", err)
	}
	defer rows.Close()

	found := map[string]MonthPoint{}
	for rows.Next() {
		var mp MonthPoint
		var in, ex int64
		if err := rows.Scan(&mp.Month, &in, &ex); err != nil {
			return nil, fmt.Errorf("scan monthly series: %w", err)
		}
		mp.Income, mp.Expense = Cents(in), Cents(ex)
		found[mp.Month] = mp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Walk backwards from this month so empty months appear as zero bars
	// rather than being skipped, which would make a gap read as a short month.
	out := make([]MonthPoint, 0, n)
	for _, key := range MonthsBack(now, n) {
		if mp, ok := found[key]; ok {
			out = append(out, mp)
		} else {
			out = append(out, MonthPoint{Month: key})
		}
	}
	return out, nil
}

// MonthsBack lists the n calendar months ending with the one containing now,
// oldest first, as YYYY-MM.
//
// The anchor is the first of the month, not now itself, because AddDate
// normalises an impossible date forwards: 31 May minus three months is 31
// February, which becomes 3 March. Stepping back from the 29th, 30th or 31st
// that way repeats some months and skips others -- on 31 May, twelve steps
// produced only seven distinct months -- which silently corrupted the
// month-by-month chart and every average taken over it for the last few days
// of most months.
func MonthsBack(now time.Time, n int) []string {
	if n <= 0 {
		return nil
	}
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, first.AddDate(0, -i, 0).Format(MonthLayout))
	}
	return out
}

// Months lists the months the user has any activity in, newest first, to
// populate the dashboard's period selector.
func (s *Store) Months(ctx context.Context, sc Scope) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT substr(occurred_on, 1, 7) AS m
		FROM transactions
		WHERE household_id = ?
		ORDER BY m DESC`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("months: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
