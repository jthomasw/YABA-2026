package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Kind is the type of a money movement.
type Kind string

const (
	// KindIncome is external money arriving. Increases cash.
	KindIncome Kind = "income"
	// KindExpense is money leaving for good. Decreases cash.
	KindExpense Kind = "expense"
	// KindFundDeposit moves cash into a savings fund.
	KindFundDeposit Kind = "fund_deposit"
	// KindFundWithdrawal moves money back out of a fund. Not income.
	KindFundWithdrawal Kind = "fund_withdrawal"
)

// Valid reports whether k is one of the four known kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindIncome, KindExpense, KindFundDeposit, KindFundWithdrawal:
		return true
	}
	return false
}

// IsTransfer reports whether k moves money between the user's own pots rather than in
// or out of their control.
func (k Kind) IsTransfer() bool {
	return k == KindFundDeposit || k == KindFundWithdrawal
}

// Label renders the kind for display.
func (k Kind) Label() string {
	switch k {
	case KindIncome:
		return "Income"
	case KindExpense:
		return "Expense"
	case KindFundDeposit:
		return "To savings"
	case KindFundWithdrawal:
		return "From savings"
	}
	return string(k)
}

// cashSign is the multiplier this kind applies to spendable cash.
const cashSignSQL = `CASE kind
	WHEN 'income'          THEN  amount_cents
	WHEN 'fund_withdrawal' THEN  amount_cents
	ELSE                        -amount_cents
END`

// txSelect is the column list and joins shared by List, All and ByID.
const txSelect = `
	SELECT t.id, t.kind, t.label, t.amount_cents, t.occurred_on, t.essential,
	       IFNULL(t.payee,''), IFNULL(t.place,''), IFNULL(t.note,''),
	       t.bucket_id, IFNULL(b.name,''),
	       t.fund_id, IFNULL(f.name, ''), IFNULL(t.receipt_path, ''),
	       IFNULL(t.receipt_name, ''), t.created_at,
	       IFNULL(NULLIF(au.display_name, ''), IFNULL(au.email, '')) AS added_by,
	       t.version
	FROM transactions t
	LEFT JOIN funds f ON f.id = t.fund_id
	LEFT JOIN expense_buckets b ON b.id = t.bucket_id
	LEFT JOIN users au ON au.id = t.user_id
`

// Transaction is one row for display.
type Transaction struct {
	ID         int64
	Kind       Kind
	Label      string // the 5W "What?"
	Amount     Cents
	OccurredOn string // the 5W "When?"
	Essential  *bool  // nil for anything that is not an expense

	// The remaining three of the wireframe's five Ws.
	Payee string // "Who?"
	Place string // "Where?"
	Note  string // "Why?"

	// BucketID attributes this transaction to a recurring monthly expense,
	// which is how a variable bucket learns what it actually costs.
	BucketID   *int64
	BucketName string

	FundID      *int64
	FundName    string
	ReceiptPath string
	ReceiptName string
	CreatedAt   string

	// AddedBy is who entered this row, for a shared budget.
	AddedBy string

	// LineItemCount is filled in by List so the table can show an expander
	// without a query per row.
	LineItemCount int

	// Items is that same breakdown in full, also filled in by List, so the log
	// can open a row and show what was actually bought. Empty for the many
	// transactions that were never split.
	Items []LineItem

	// Version increments on every edit. The form carries it back so a save can
	// be refused if somebody else edited the row in the meantime.
	Version int64
}

// HasDetail reports whether any of the optional 5W fields were filled in, so
// the UI can hide an empty detail block.
func (t Transaction) HasDetail() bool {
	return t.Payee != "" || t.Place != "" || t.Note != ""
}

// Split reports whether this transaction was broken into line items.
func (t Transaction) Split() bool { return t.LineItemCount > 0 }

// SignedAmount returns the effect on cash, for templates that colour a row.
func (t Transaction) SignedAmount() Cents {
	if t.Kind == KindIncome || t.Kind == KindFundWithdrawal {
		return t.Amount
	}
	return -t.Amount
}

// EssentialText renders the essential flag for a table cell.
func (t Transaction) EssentialText() string {
	if t.Essential == nil {
		return "—"
	}
	if *t.Essential {
		return "Essential"
	}
	return "Non-essential"
}

// NewTransaction is the input for creating or editing a row.
type NewTransaction struct {
	Kind        Kind
	Label       string
	Amount      Cents
	OccurredOn  string
	Essential   *bool
	Payee       string
	Place       string
	Note        string
	BucketID    *int64
	FundID      *int64
	ReceiptPath string
	ReceiptName string

	// Version is the version the editor was shown, used as a compare-and-swap on update.
	Version int64
}

// Add inserts a plain income or expense. Fund movements cannot be created here: they
// go through Deposit, Withdraw or CloseFund, which enforce the balance rules inside a
// transaction, so a handler cannot invent a deposit.
func (s *Store) Add(ctx context.Context, sc Scope, n NewTransaction) (int64, error) {
	return s.add(ctx, sc, n, 0)
}

// AddWithReceiptJob creates an expense and claims its completed receipt in the
// same transaction.  A receipt can therefore never create two expenses when
// two browser requests submit the same form at once.
func (s *Store) AddWithReceiptJob(ctx context.Context, sc Scope, n NewTransaction, jobID int64) (int64, error) {
	if jobID == 0 {
		return s.Add(ctx, sc, n)
	}
	return s.add(ctx, sc, n, jobID)
}

func (s *Store) add(ctx context.Context, sc Scope, n NewTransaction, receiptJobID int64) (int64, error) {
	if n.Kind != KindIncome && n.Kind != KindExpense {
		return 0, fmt.Errorf("Add only accepts income or expense, got %q", n.Kind)
	}
	if n.Amount <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}

	// An expense always records the essential flag; income never does.
	var essential any
	if n.Kind == KindExpense {
		v := true
		if n.Essential != nil {
			v = *n.Essential
		}
		essential = boolToInt(v)
	}

	bucket, err := s.resolveBucket(ctx, sc, n.BucketID)
	if err != nil {
		return 0, err
	}

	// household_id owns the row and user_id records who entered it.
	var newID int64
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if receiptJobID != 0 {
			var path, name string
			err := tx.QueryRowContext(ctx, `
				SELECT path, original_name FROM receipt_jobs
				WHERE id = ? AND household_id = ? AND transaction_id IS NULL
				  AND `+receiptSettledSQL,
				receiptJobID, sc.HouseholdID).Scan(&path, &name)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read receipt job: %w", err)
			}
			n.ReceiptPath, n.ReceiptName = path, name
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO transactions
				(household_id, user_id, kind, label, amount_cents, occurred_on, essential,
				 payee, place, note, bucket_id, receipt_path, receipt_name)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sc.HouseholdID, sc.UserID, string(n.Kind), cleanLabel(n.Label), int64(n.Amount), n.OccurredOn, essential,
			cleanLabel(n.Payee), cleanLabel(n.Place), cleanNote(n.Note), bucket,
			nullIfEmpty(n.ReceiptPath), nullIfEmpty(n.ReceiptName),
		)
		if err != nil {
			return fmt.Errorf("insert transaction: %w", err)
		}
		newID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if receiptJobID != 0 {
			res, err := tx.ExecContext(ctx, `
				UPDATE receipt_jobs SET transaction_id = ?
				WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
				newID, receiptJobID, sc.HouseholdID)
			if err != nil {
				return fmt.Errorf("mark receipt attached: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrNotFound
			}
		}
		return recordAudit(ctx, tx, sc, "created", "transaction", newID,
			fmt.Sprintf("%s %s — %q on %s",
				n.Kind.Label(), n.Amount.Display(), cleanLabel(n.Label), n.OccurredOn))
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// updateInTx is the body of Update, separated so that UpdateWithItems can run it
// and the line-item replacement inside ONE database transaction.
//
// They used to be two, and the gap between them was a real hole: editing a
// $100.00 expense split Food $60.00 / Books $40.00 down to $50.00 committed the
// new amount, and if the second call did not run -- a cancelled request context
// is enough -- the old lines stayed. The dashboard's headline spend then read
// $50.00 while its own category breakdown read $100.00, with nothing in the
// schema to catch it, because the sum is only ever checked at the moment the
// items are written.
func updateInTx(
	ctx context.Context,
	tx *sql.Tx,
	sc Scope,
	id int64,
	n NewTransaction,
	essential any,
	bucket any,
) error {
	{
		// What it said before, so the history records the change rather than only the
		// outcome.
		var wasLabel string
		var wasCents int64
		if err := tx.QueryRowContext(ctx,
			`SELECT label, amount_cents FROM transactions WHERE id = ? AND household_id = ?`,
			id, sc.HouseholdID).Scan(&wasLabel, &wasCents); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read transaction before update: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE transactions
			SET kind = ?, label = ?, amount_cents = ?, occurred_on = ?, essential = ?,
			    payee = ?, place = ?, note = ?, bucket_id = ?, version = version + 1
			WHERE id = ? AND household_id = ? AND kind IN ('income','expense')
			  AND (? = 0 OR version = ?)`,
			string(n.Kind), cleanLabel(n.Label), int64(n.Amount), n.OccurredOn, essential,
			cleanLabel(n.Payee), cleanLabel(n.Place), cleanNote(n.Note), bucket,
			id, sc.HouseholdID, n.Version, n.Version,
		)
		if err != nil {
			return fmt.Errorf("update transaction: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return explainFailedUpdate(ctx, tx, sc, id)
		}

		summary := fmt.Sprintf("%q %s", cleanLabel(n.Label), n.Amount.Display())
		if wasLabel != cleanLabel(n.Label) || wasCents != int64(n.Amount) {
			summary = fmt.Sprintf("%q %s → %q %s",
				wasLabel, Cents(wasCents).Display(), cleanLabel(n.Label), n.Amount.Display())
		}
		return recordAudit(ctx, tx, sc, "edited", "transaction", id, summary)
	}
}

// UpdateWithItems saves an edit and its breakdown atomically.
//
// Either the new amount and the new lines are both stored, or neither is. That
// is the only thing keeping SUM(line_items) == transactions.amount_cents true,
// since the check lives in the write rather than in a constraint.
//
// A nil or empty slice clears the breakdown, which is how somebody removes line
// items: they blank the rows and save.
func (s *Store) UpdateWithItems(
	ctx context.Context,
	sc Scope,
	id int64,
	n NewTransaction,
	items []NewLineItem,
) error {
	if n.Kind != KindIncome && n.Kind != KindExpense {
		return fmt.Errorf("UpdateWithItems only accepts income or expense, got %q", n.Kind)
	}
	if n.Amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	var essential any
	if n.Kind == KindExpense {
		v := true
		if n.Essential != nil {
			v = *n.Essential
		}
		essential = boolToInt(v)
	}

	bucket, err := s.resolveBucket(ctx, sc, n.BucketID)
	if err != nil {
		return err
	}

	if n.Version == 0 {
		log.Printf("store: transaction %d updated with no version; staleness check skipped", id)
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := updateInTx(ctx, tx, sc, id, n, essential, bucket); err != nil {
			return err
		}
		return setLineItemsInTx(ctx, tx, sc, id, items)
	})
}

// explainFailedUpdate works out why an UPDATE matched nothing: the row is gone,
// somebody else changed it, or it is not an editable kind.
func explainFailedUpdate(ctx context.Context, tx *sql.Tx, sc Scope, id int64) error {
	var kind Kind
	var version int64
	err := tx.QueryRowContext(ctx,
		`SELECT kind, version FROM transactions WHERE id = ? AND household_id = ?`,
		id, sc.HouseholdID).Scan(&kind, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("update transaction: %w", err)
	}
	if kind.IsTransfer() {
		return ErrNotFound
	}
	// The row is there and editable, so the version must have moved.
	return ErrConflict
}

// ByID fetches a single transaction the user owns.
func (s *Store) ByID(ctx context.Context, sc Scope, id int64) (Transaction, error) {
	row := s.db.QueryRowContext(ctx, `
		`+txSelect+`
		WHERE t.id = ? AND t.household_id = ?`, id, sc.HouseholdID)

	t, err := scanTransaction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Transaction{}, ErrNotFound
	}
	return t, err
}

// Delete removes an income or expense and records what it was.
//
// It returns the stored receipt files nothing refers to any more, for the
// caller to delete from disk once the database change has committed. Before,
// the file outlived its row for good -- and the receipt job that produced the
// expense lost its link (ON DELETE SET NULL) and reappeared on the Receipts
// page as if it were still waiting, pointing at a receipt already entered and
// deleted. The job is removed with the expense now.
func (s *Store) Delete(ctx context.Context, sc Scope, id int64) (orphaned []string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// The job goes first: deleting the transaction would set its link to
		// NULL, and the job could then no longer be found by it. The kind check
		// matches the DELETE below, so a fund transfer's id removes nothing.
		jobPaths, err := queryPaths(ctx, tx, `
			DELETE FROM receipt_jobs
			WHERE transaction_id = ? AND household_id = ?
			  AND EXISTS (SELECT 1 FROM transactions
			              WHERE id = ? AND household_id = ? AND kind IN ('income','expense'))
			RETURNING path`,
			id, sc.HouseholdID, id, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("delete receipt job: %w", err)
		}

		var kind Kind
		var label, on, receipt string
		var cents int64
		err = tx.QueryRowContext(ctx, `
			DELETE FROM transactions
			WHERE id = ? AND household_id = ? AND kind IN ('income','expense')
			RETURNING kind, label, amount_cents, occurred_on, IFNULL(receipt_path, '')`,
			id, sc.HouseholdID).Scan(&kind, &label, &cents, &on, &receipt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete transaction: %w", err)
		}
		if orphaned, err = unreferenced(ctx, tx, append(jobPaths, receipt)); err != nil {
			return err
		}

		return recordAudit(ctx, tx, sc, "deleted", "transaction", id,
			fmt.Sprintf("%s %s — %q on %s",
				kind.Label(), Cents(cents).Display(), label, on))
	})
	if err != nil {
		return nil, err
	}
	return orphaned, nil
}

// queryPaths runs a query returning one path per row (a SELECT, or a DELETE
// ... RETURNING path) and collects them.
func queryPaths(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// unreferenced returns the distinct, non-empty paths that no transaction and
// no receipt job still points at, so deleting their files cannot break a row
// that remains.
func unreferenced(ctx context.Context, tx *sql.Tx, paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM transactions WHERE receipt_path = ?)
			     + (SELECT COUNT(*) FROM receipt_jobs WHERE path = ?)`, p, p).Scan(&n); err != nil {
			return nil, fmt.Errorf("check receipt references: %w", err)
		}
		if n == 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

// ── listing ───────────────────────────────────────────────────────────────────

// Filter narrows a transaction list.
type Filter struct {
	Kind     Kind   // "" for all kinds
	Month    string // "" for all time, else YYYY-MM
	Search   string // "" for no text filter
	Limit    int    // 0 means DefaultPageSize
	Offset   int
	Transfer string // "hide" to omit fund movements
}

// DefaultPageSize bounds a transaction page.
const DefaultPageSize = 25

// where builds the shared WHERE clause and its arguments.
func (f Filter) where(householdID int64) (string, []any, error) {
	clauses := []string{"t.household_id = ?"}
	args := []any{householdID}

	if f.Kind != "" {
		if !f.Kind.Valid() {
			return "", nil, fmt.Errorf("unknown transaction type %q", f.Kind)
		}
		clauses = append(clauses, "t.kind = ?")
		args = append(args, string(f.Kind))
	}
	if f.Transfer == "hide" {
		clauses = append(clauses, "t.kind IN ('income','expense')")
	}
	if f.Month != "" {
		start, end, err := monthRange(f.Month)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "t.occurred_on >= ? AND t.occurred_on < ?")
		args = append(args, start, end)
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		// ESCAPE makes a literal % or _ in the user's search text match
		// itself instead of acting as a wildcard.
		clauses = append(clauses, `t.label LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q)+"%")
	}
	return strings.Join(clauses, " AND "), args, nil
}

// List returns one page of transactions plus the total number matching the
// filter, so the template can render "showing 1-25 of 340".
func (s *Store) List(ctx context.Context, sc Scope, f Filter) ([]Transaction, int, error) {
	where, args, err := f.where(sc.HouseholdID)
	if err != nil {
		return nil, 0, err
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transactions t WHERE `+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count transactions: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// Ordered by created_at then id, not occurred_on: somebody who backdates an entry
	// still expects to see it at the top immediately after saving.
	rows, err := s.db.QueryContext(ctx, `
		`+txSelect+`
		WHERE `+where+`
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	var out []Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan transactions: %w", err)
	}

	// One extra query for the whole page, rather than one per row, so the list
	// can expand a split transaction into the things that were bought without
	// turning a page of twenty rows into twenty-one queries.
	ids := make([]int64, 0, len(out))
	for _, t := range out {
		ids = append(ids, t.ID)
	}
	items, err := s.LineItemsFor(ctx, sc, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].Items = items[out[i].ID]
		out[i].LineItemCount = len(out[i].Items)
	}

	return out, total, nil
}

// All returns every matching transaction with no page limit.
func (s *Store) All(ctx context.Context, sc Scope, f Filter) ([]Transaction, error) {
	f.Limit = -1
	f.Offset = 0
	where, args, err := f.where(sc.HouseholdID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		`+txSelect+`
		WHERE `+where+`
		ORDER BY t.occurred_on DESC, t.id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("export transactions: %w", err)
	}
	defer rows.Close()

	var out []Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

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

	q := `
		SELECT COALESCE(NULLIF(TRIM(label), ''), ?) AS grp,
		       SUM(amount_cents)
		FROM transactions
		WHERE household_id = ? AND kind = ?` + clause
	args := append([]any{Uncategorised, sc.HouseholdID, string(kind)}, span...)
	q += ` GROUP BY grp ORDER BY SUM(amount_cents) DESC`

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

// ── scanning helpers ──────────────────────────────────────────────────────────

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransaction(row rowScanner) (Transaction, error) {
	var t Transaction
	var kind string
	var amount int64
	var essential sql.NullInt64
	var fundID, bucketID sql.NullInt64

	// The order here must match txSelect exactly.
	err := row.Scan(&t.ID, &kind, &t.Label, &amount, &t.OccurredOn, &essential,
		&t.Payee, &t.Place, &t.Note, &bucketID, &t.BucketName,
		&fundID, &t.FundName, &t.ReceiptPath, &t.ReceiptName, &t.CreatedAt,
		&t.AddedBy, &t.Version)
	if err != nil {
		return Transaction{}, err
	}
	if bucketID.Valid {
		v := bucketID.Int64
		t.BucketID = &v
	}

	t.Kind = Kind(kind)
	t.Amount = Cents(amount)
	if essential.Valid {
		v := essential.Int64 == 1
		t.Essential = &v
	}
	if fundID.Valid {
		v := fundID.Int64
		t.FundID = &v
	}
	return t, nil
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// cleanLabel trims a label and caps its length.
func cleanLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// Truncate by rune, not byte: slicing a byte string mid-character would
	// emit invalid UTF-8 and render as a replacement glyph.
	const max = 60
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// escapeLike neutralises LIKE metacharacters in user search text.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// resolveBucket validates a bucket id supplied by a form: nil for no bucket, or the id.
func (s *Store) resolveBucket(ctx context.Context, sc Scope, bucketID *int64) (any, error) {
	if bucketID == nil || *bucketID <= 0 {
		return nil, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM expense_buckets
		 WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
		*bucketID, sc.HouseholdID).Scan(&n)
	if err != nil {
		return nil, fmt.Errorf("verify bucket: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return *bucketID, nil
}

// cleanNote trims a free-text note and caps it.
func cleanNote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	const max = 500
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}
