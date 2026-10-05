package store

import (
	"context"
	"database/sql"
	"fmt"
)

// LineItem is one entry within a transaction: a single shop trip is one transaction
// but may be groceries, cleaning products and a magazine.
type LineItem struct {
	ID          int64
	Description string
	Category    string
	Amount      Cents
	Position    int
}

// NewLineItem is the input for one line.
type NewLineItem struct {
	Description string
	Category    string
	Amount      Cents
}

// SetLineItems replaces a transaction's lines. They must sum exactly to the
// transaction's amount, or the category breakdown and the headline total would tell
// different stories with no way to know which is right.
func (s *Store) SetLineItems(ctx context.Context, sc Scope, txID int64, items []NewLineItem) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return setLineItemsInTx(ctx, tx, sc, txID, items)
	})
}

// setLineItemsInTx is the body of SetLineItems, separated so an edit can replace
// the amount and the breakdown in a single database transaction. See
// UpdateWithItems.
func setLineItemsInTx(ctx context.Context, tx *sql.Tx, sc Scope, txID int64, items []NewLineItem) error {
	{
		var total Cents
		var kind string
		err := tx.QueryRowContext(ctx,
			`SELECT amount_cents, kind FROM transactions WHERE id = ? AND household_id = ?`,
			txID, sc.HouseholdID).Scan(&total, &kind)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read transaction: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM line_items WHERE transaction_id = ?`, txID); err != nil {
			return fmt.Errorf("clear line items: %w", err)
		}
		if len(items) == 0 {
			return nil
		}

		// A negative line is a discount or coupon, which real receipts carry. Only
		// zero is meaningless. The lines must still add up to the (positive) total.
		var sum Cents
		for _, it := range items {
			if it.Amount == 0 {
				return fmt.Errorf("every line item needs an amount (use a minus sign for a discount)")
			}
			sum += it.Amount
		}
		if sum != total {
			return fmt.Errorf("%w: they add up to %s but the transaction is %s",
				ErrItemsDoNotBalance, sum.Display(), total.Display())
		}

		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO line_items(transaction_id, description, category, amount_cents, position)
			VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare line item: %w", err)
		}
		defer stmt.Close()

		for i, it := range items {
			if _, err := stmt.ExecContext(ctx, txID,
				cleanLabel(it.Description), cleanLabel(it.Category), int64(it.Amount), i); err != nil {
				return fmt.Errorf("insert line item: %w", err)
			}
		}
		return nil
	}
}

// LineItems returns one transaction's lines.
func (s *Store) LineItems(ctx context.Context, sc Scope, txID int64) ([]LineItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT li.id, li.description, li.category, li.amount_cents, li.position
		FROM line_items li
		JOIN transactions t ON t.id = li.transaction_id
		WHERE li.transaction_id = ? AND t.household_id = ?
		ORDER BY li.position ASC, li.id ASC`, txID, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("line items: %w", err)
	}
	defer rows.Close()

	out := []LineItem{}
	for rows.Next() {
		var it LineItem
		var amount int64
		if err := rows.Scan(&it.ID, &it.Description, &it.Category, &amount, &it.Position); err != nil {
			return nil, fmt.Errorf("scan line item: %w", err)
		}
		it.Amount = Cents(amount)
		out = append(out, it)
	}
	return out, rows.Err()
}

// LineItemsFor returns the lines of many transactions at once, keyed by
// transaction id, so the log can expand any row without a query per row: one
// query for the whole page, however many rows it shows.
func (s *Store) LineItemsFor(ctx context.Context, sc Scope, txIDs []int64) (map[int64][]LineItem, error) {
	out := map[int64][]LineItem{}
	if len(txIDs) == 0 {
		return out, nil
	}

	// Placeholders rather than interpolated ids, as above.
	ph := make([]byte, 0, len(txIDs)*2)
	args := make([]any, 0, len(txIDs)+1)
	args = append(args, sc.HouseholdID)
	for i, id := range txIDs {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT li.transaction_id, li.id, li.description, li.category,
		       li.amount_cents, li.position
		FROM line_items li
		JOIN transactions t ON t.id = li.transaction_id
		WHERE t.household_id = ? AND li.transaction_id IN (`+string(ph)+`)
		ORDER BY li.transaction_id, li.position ASC, li.id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("line items for transactions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var txID int64
		var it LineItem
		var amount int64
		if err := rows.Scan(&txID, &it.ID, &it.Description, &it.Category,
			&amount, &it.Position); err != nil {
			return nil, fmt.Errorf("scan line item: %w", err)
		}
		it.Amount = Cents(amount)
		out[txID] = append(out[txID], it)
	}
	return out, rows.Err()
}

// spendingCTE is the one definition of "which category does this spending
// count towards", shared by CategoryBreakdown and ListBudgets so the chart and
// the caps cannot disagree again. They used to: the breakdown counted line-item
// categories while budgets matched only the transaction label, so a Costco shop
// split into Food lines showed under Food in the chart and never touched the
// Food budget.
//
// It yields one row per piece of an expense in the household:
//
//   - a transaction broken into lines contributes each line, under the line's
//     category -- or, when a line's category is blank, the transaction's own
//     label, because an unlabelled line on a "Costco" receipt is still Costco
//     spending, and filing it as Uncategorised hid it from a Costco budget;
//   - a transaction with no lines contributes itself, under its label;
//   - whatever is still blank is Uncategorised.
//
// The LEFT JOIN does both cases in one pass: a transaction without lines joins
// one all-NULL row. Line items always sum to their transaction (see
// SetLineItems), so either way every cent is counted exactly once.
//
// It binds two parameters, in order: Uncategorised and the household id.
// spendingArgs returns them.
const spendingCTE = `
	WITH spending AS (
		SELECT t.occurred_on AS occurred_on,
		       COALESCE(NULLIF(TRIM(li.category), ''), NULLIF(TRIM(t.label), ''), ?) AS category,
		       CASE WHEN li.id IS NULL THEN t.amount_cents ELSE li.amount_cents END AS amount
		FROM transactions t
		LEFT JOIN line_items li ON li.transaction_id = t.id
		WHERE t.household_id = ? AND t.kind = 'expense'
	)`

// spendingArgs is the argument list spendingCTE's placeholders consume.
func spendingArgs(sc Scope) []any {
	return []any{Uncategorised, sc.HouseholdID}
}

// CategoryBreakdown totals spending by category, largest first, by the rule
// spendingCTE defines.
func (s *Store) CategoryBreakdown(ctx context.Context, sc Scope, month string) ([]LabelTotal, error) {
	clause, span, err := monthClause(month, "")
	if err != nil {
		return nil, err
	}
	args := append(spendingArgs(sc), span...)

	rows, err := s.db.QueryContext(ctx, spendingCTE+`
		SELECT category, SUM(amount) AS total
		FROM spending
		WHERE 1 = 1`+clause+`
		GROUP BY category
		ORDER BY total DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("category breakdown: %w", err)
	}
	defer rows.Close()

	out := []LabelTotal{}
	for rows.Next() {
		var lt LabelTotal
		var v int64
		if err := rows.Scan(&lt.Label, &v); err != nil {
			return nil, fmt.Errorf("scan category breakdown: %w", err)
		}
		lt.Total = Cents(v)
		out = append(out, lt)
	}
	return out, rows.Err()
}
