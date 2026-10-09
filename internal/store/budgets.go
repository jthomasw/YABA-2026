package store

import (
	"context"
	"fmt"
	"strings"
)

// Budget is a monthly spending cap for one category, joined against what was actually
// spent in the period.
type Budget struct {
	ID       int64
	Category string
	Limit    Cents
	Spent    Cents
}

// Remaining is what is left of the cap, negative once overspent.
func (b Budget) Remaining() Cents { return b.Limit - b.Spent }

// Over reports whether the cap has been breached.
func (b Budget) Over() bool { return b.Spent > b.Limit }

// Progress is spend as a percentage of the cap, clamped to 100 so a bar never
// overflows its track. Use Over to signal the breach instead.
func (b Budget) Progress() float64 { return Ratio(b.Spent, b.Limit) }

// Status classifies the budget for styling: "ok", "warn" past 80%, or "over".
func (b Budget) Status() string {
	switch {
	case b.Over():
		return "over"
	case b.Limit > 0 && float64(b.Spent) >= 0.8*float64(b.Limit):
		return "warn"
	default:
		return "ok"
	}
}

// ListBudgets returns every budget with the spend for the given month.
func (s *Store) ListBudgets(ctx context.Context, sc Scope, month string) ([]Budget, error) {
	start, end, err := monthRange(month)
	if err != nil {
		return nil, err
	}
	if month == "" {
		// With no month selected a cap is meaningless, so compare against the
		// current calendar month rather than all of history.
		start, end, err = monthRange(Today()[:7])
		if err != nil {
			return nil, err
		}
	}

	// Spending is counted by the same rule as the category chart (see
	// spendingCTE), so line items count against the budget for their own
	// category. The match ignores case, as it always has: a "food" line and a
	// "Food" budget are the same thing to the person who typed them.
	args := append(spendingArgs(sc), start, end, sc.HouseholdID)
	rows, err := s.db.QueryContext(ctx, spendingCTE+`
		SELECT b.id, b.category, b.limit_cents,
		       IFNULL((
		           SELECT SUM(sp.amount)
		           FROM spending sp
		           WHERE LOWER(sp.category) = LOWER(TRIM(b.category))
		             AND sp.occurred_on >= ? AND sp.occurred_on < ?
		       ), 0) AS spent
		FROM budgets b
		WHERE b.household_id = ?
		ORDER BY b.category COLLATE NOCASE ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()

	out := []Budget{}
	for rows.Next() {
		var b Budget
		var limit, spent int64
		if err := rows.Scan(&b.ID, &b.Category, &limit, &spent); err != nil {
			return nil, fmt.Errorf("scan budget: %w", err)
		}
		b.Limit, b.Spent = Cents(limit), Cents(spent)
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBudget creates or updates the cap for a category.
func (s *Store) SetBudget(ctx context.Context, sc Scope, category string, limit Cents) error {
	category = cleanLabel(category)
	if category == "" {
		return fmt.Errorf("budget needs a category")
	}
	if limit <= 0 {
		return fmt.Errorf("budget must be greater than zero")
	}

	// ON CONFLICT keyed on the UNIQUE(household_id, category) index makes this
	// idempotent, so submitting the form twice updates rather than erroring.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO budgets(household_id, user_id, category, limit_cents) VALUES(?, ?, ?, ?)
		ON CONFLICT(household_id, category) DO UPDATE SET limit_cents = excluded.limit_cents`,
		sc.HouseholdID, sc.UserID, category, int64(limit))
	if err != nil {
		return fmt.Errorf("set budget: %w", err)
	}
	return nil
}

// DeleteBudget removes a cap.
func (s *Store) DeleteBudget(ctx context.Context, sc Scope, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM budgets WHERE id = ? AND household_id = ?`, id, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("delete budget: %w", err)
	}
	return requireOneRow(res)
}

// SpendCategories lists the expense labels actually used, newest first, to populate a
// datalist on the expense and budget forms.
func (s *Store) SpendCategories(ctx context.Context, sc Scope) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT TRIM(label)
		FROM transactions
		WHERE household_id = ? AND kind = 'expense' AND TRIM(label) <> ''
		GROUP BY LOWER(TRIM(label))
		ORDER BY MAX(occurred_on) DESC, SUM(amount_cents) DESC
		LIMIT 50`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("spend categories: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}
