// Package store is the only package that speaks SQL.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
)

// Cents is an alias so callers of this package do not have to import money just to name
// a field type.
type Cents = money.Cents

// Ratio is re-exported for the same reason as Cents.
var Ratio = money.Ratio

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Scope says whose money a call operates on: the household that owns the data, and the
// person performing the action.
type Scope struct {
	HouseholdID int64
	UserID      int64
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Sentinel errors. Handlers map these to HTTP responses; they must never leak
// raw SQL text to a user.
var (
	// ErrNotFound covers "no such row" and "that row belongs to someone else".
	ErrNotFound = errors.New("not found")

	// ErrConflict means the row changed between being read and being saved.
	ErrConflict = errors.New("changed by someone else")

	// ErrEmailTaken is returned instead of a raw UNIQUE constraint error.
	ErrEmailTaken = errors.New("that email already has an account")

	// ErrInsufficientCash blocks moving more into a fund than the user holds.
	ErrInsufficientCash = errors.New("not enough available cash")

	// ErrInsufficientFund blocks withdrawing more than a fund contains.
	ErrInsufficientFund = errors.New("not enough money in that fund")
)

// Today returns the current date as YYYY-MM-DD.
func Today() string {
	return time.Now().Format(DateLayout)
}

// DateLayout is the storage format for occurred_on.
const DateLayout = "2006-01-02"

// MonthLayout is the storage format for a month selector, e.g. "2026-04".
const MonthLayout = "2006-01"

// ParseDate validates a user-supplied date. Without it a hand-edited form could put
// tomorrow, or nothing at all, into a column every chart sorts on.
func ParseDate(s string) (string, error) {
	if s == "" {
		return Today(), nil
	}
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return "", fmt.Errorf("date must look like YYYY-MM-DD")
	}
	// Guard against fat-fingered years such as 20026, which would push a row
	// to the end of every ordered result set forever.
	if y := t.Year(); y < 1900 || y > 2200 {
		return "", fmt.Errorf("date year is out of range")
	}
	return t.Format(DateLayout), nil
}

// monthRange converts 2026-04 into the half-open interval [2026-04-01, 2026-05-01).
func monthRange(month string) (start, end string, err error) {
	if month == "" {
		return "", "", nil
	}
	t, err := time.Parse(MonthLayout, month)
	if err != nil {
		return "", "", fmt.Errorf("month must look like YYYY-MM")
	}
	return t.Format(DateLayout), t.AddDate(0, 1, 0).Format(DateLayout), nil
}

// monthClause is the optional " AND occurred_on >= ? AND occurred_on < ?" that
// narrows a query to one month, with its two bound values; both are empty for
// all time. prefix qualifies the column, e.g. "t.".
func monthClause(month, prefix string) (clause string, args []any, err error) {
	if month == "" {
		return "", nil, nil
	}
	start, end, err := monthRange(month)
	if err != nil {
		return "", nil, err
	}
	return " AND " + prefix + "occurred_on >= ? AND " + prefix + "occurred_on < ?", []any{start, end}, nil
}

// LabelTotal is one slice of a breakdown chart.
type LabelTotal struct {
	Label string
	Total Cents
}

// ═════════════════════════════════════════════════════════════════════════════
// transactions.go
// ═════════════════════════════════════════════════════════════════════════════

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
		return recordAudit(ctx, tx, sc, "created", "transaction", newID,
			fmt.Sprintf("%s %s — %q on %s",
				n.Kind.Label(), n.Amount.Display(), cleanLabel(n.Label), n.OccurredOn))
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// Update edits an income or expense in place. Without it, correcting a typo meant
// deleting and retyping, which lost created_at and reordered the user's history.
func (s *Store) Update(ctx context.Context, sc Scope, id int64, n NewTransaction) error {
	if n.Kind != KindIncome && n.Kind != KindExpense {
		return fmt.Errorf("Update only accepts income or expense, got %q", n.Kind)
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
		// A zero version skips the compare-and-swap in the UPDATE below, so this
		// save cannot be refused for being stale. The column defaults to 1 and
		// only ever climbs, so no real row is version 0 and every edit form
		// carries one: a save arriving without it did not come from one of our
		// pages. The hatch stays because silently rejecting such a save would be
		// worse than taking it, but a lost update is invisible by nature and
		// needs to leave a trace. If this line never appears, the hatch can go.
		log.Printf("store: transaction %d updated with no version; staleness check skipped", id)
	}

	// The WHERE clause carries household_id as well as id, so a guessed id belonging to
	// someone else affects zero rows.
	err = s.inTx(ctx, func(tx *sql.Tx) error {
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
	})
	return err
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
func (s *Store) Delete(ctx context.Context, sc Scope, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var kind Kind
		var label string
		var cents int64
		var on string
		err := tx.QueryRowContext(ctx, `
			DELETE FROM transactions
			WHERE id = ? AND household_id = ? AND kind IN ('income','expense')
			RETURNING kind, label, amount_cents, occurred_on`,
			id, sc.HouseholdID).Scan(&kind, &label, &cents, &on)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete transaction: %w", err)
		}
		return recordAudit(ctx, tx, sc, "deleted", "transaction", id,
			fmt.Sprintf("%s %s — %q on %s",
				kind.Label(), Cents(cents).Display(), label, on))
	})
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
	// can show a "3 items" expander on split transactions.
	ids := make([]int64, 0, len(out))
	for _, t := range out {
		ids = append(ids, t.ID)
	}
	counts, err := s.LineItemCounts(ctx, sc, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].LineItemCount = counts[out[i].ID]
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
		SELECT CASE WHEN TRIM(label) = '' THEN 'Uncategorised' ELSE TRIM(label) END AS grp,
		       SUM(amount_cents)
		FROM transactions
		WHERE household_id = ? AND kind = ?` + clause
	args := append([]any{sc.HouseholdID, string(kind)}, span...)
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
func (s *Store) MonthlySeries(ctx context.Context, sc Scope, n int) ([]MonthPoint, error) {
	return s.MonthlySeriesAsOf(ctx, sc, n, time.Now())
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

// ═════════════════════════════════════════════════════════════════════════════
// funds.go
// ═════════════════════════════════════════════════════════════════════════════

// Fund is a savings pot. Balance is always derived from transactions and is
// never read from a column, let alone from a form field.
type Fund struct {
	ID           int64
	Name         string
	Goal         Cents
	TargetMonths int
	Balance      Cents
	CreatedAt    string

	// IsEmergency marks the one fund the Emergency Fund dashboard tab tracks.
	IsEmergency bool
}

// Remaining is how much more is needed to hit the goal, floored at zero.
func (f Fund) Remaining() Cents {
	if f.Goal <= f.Balance {
		return 0
	}
	return f.Goal - f.Balance
}

// Progress is the percentage of the goal reached, clamped to 0..100.
func (f Fund) Progress() float64 {
	return Ratio(f.Balance, f.Goal)
}

// Complete reports whether the goal has been met.
func (f Fund) Complete() bool {
	return f.Goal > 0 && f.Balance >= f.Goal
}

// HasGoal reports whether a target has been set, for templates that hide the
// progress bar when there is nothing to progress towards.
func (f Fund) HasGoal() bool { return f.Goal > 0 }

// MonthlyNeeded is the amount per month required to reach the goal within TargetMonths,
// and 0 when either the goal or the horizon is unset.
func (f Fund) MonthlyNeeded() Cents {
	if f.TargetMonths <= 0 || f.Remaining() <= 0 {
		return 0
	}
	// Round up: paying the floor every month would land a cent short.
	per := math.Ceil(float64(f.Remaining()) / float64(f.TargetMonths))
	return Cents(per)
}

// balanceSQL derives a fund's balance from its own transactions.
const balanceSQL = `
	IFNULL((
		SELECT SUM(CASE t.kind
			WHEN 'fund_deposit'    THEN  t.amount_cents
			WHEN 'fund_withdrawal' THEN -t.amount_cents
			ELSE 0 END)
		FROM transactions t
		WHERE t.fund_id = f.id
	), 0)`

const fundColumns = `f.id, f.name, f.goal_cents, f.target_months, ` + balanceSQL + `, f.created_at, f.is_emergency`

// ListFunds returns the user's open funds, newest goal progress included.
func (s *Store) ListFunds(ctx context.Context, sc Scope) ([]Fund, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+fundColumns+`
		FROM funds f
		WHERE f.household_id = ? AND f.closed_at IS NULL
		ORDER BY f.id ASC`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("list funds: %w", err)
	}
	defer rows.Close()

	out := []Fund{}
	for rows.Next() {
		f, err := scanFund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FundByID returns one open fund the user owns.
func (s *Store) FundByID(ctx context.Context, sc Scope, fundID int64) (Fund, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+fundColumns+`
		FROM funds f
		WHERE f.id = ? AND f.household_id = ? AND f.closed_at IS NULL`, fundID, sc.HouseholdID)
	f, err := scanFund(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Fund{}, ErrNotFound
	}
	return f, err
}

// CreateFund adds a savings pot.
func (s *Store) CreateFund(ctx context.Context, sc Scope, name string, goal Cents, targetMonths int) (int64, error) {
	name = cleanFundName(name)
	if name == "" {
		return 0, fmt.Errorf("fund needs a name")
	}
	if goal < 0 {
		return 0, fmt.Errorf("goal cannot be negative")
	}
	if targetMonths < 0 || targetMonths > 600 {
		return 0, fmt.Errorf("target months must be between 0 and 600")
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO funds(household_id, user_id, name, goal_cents, target_months) VALUES(?, ?, ?, ?, ?)`,
		sc.HouseholdID, sc.UserID, name, int64(goal), targetMonths)
	if err != nil {
		return 0, fmt.Errorf("create fund: %w", err)
	}
	return res.LastInsertId()
}

// UpdateFundGoal changes a fund's target amount and horizon.
func (s *Store) UpdateFundGoal(ctx context.Context, sc Scope, fundID int64, goal Cents, targetMonths int) error {
	if goal < 0 {
		return fmt.Errorf("goal cannot be negative")
	}
	if targetMonths < 0 || targetMonths > 600 {
		return fmt.Errorf("target months must be between 0 and 600")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE funds SET goal_cents = ?, target_months = ?
		 WHERE id = ? AND household_id = ? AND closed_at IS NULL`,
		int64(goal), targetMonths, fundID, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("update fund goal: %w", err)
	}
	return requireOneRow(res)
}

// RenameFund changes a fund's display name.
func (s *Store) RenameFund(ctx context.Context, sc Scope, fundID int64, name string) error {
	name = cleanFundName(name)
	if name == "" {
		return fmt.Errorf("fund needs a name")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE funds SET name = ? WHERE id = ? AND household_id = ? AND closed_at IS NULL`,
		name, fundID, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("rename fund: %w", err)
	}
	return requireOneRow(res)
}

// Deposit moves cash into a fund: one row, inserted inside a transaction that first
// re-reads available cash, so a user cannot move in more than they hold and the cash
// side and the fund side cannot disagree.
func (s *Store) Deposit(ctx context.Context, sc Scope, fundID int64, amount Cents, occurredOn string) error {
	if amount <= 0 {
		return fmt.Errorf("deposit must be positive")
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := fundExists(ctx, tx, sc.HouseholdID, fundID); err != nil {
			return err
		}

		cash, err := cashInTx(ctx, tx, sc.HouseholdID)
		if err != nil {
			return err
		}
		if amount > cash {
			return fmt.Errorf("%w: you have %s available", ErrInsufficientCash, cash.Display())
		}

		name, err := fundNameInTx(ctx, tx, fundID)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO transactions(household_id, user_id, kind, label, amount_cents, occurred_on, fund_id)
			VALUES (?, ?, 'fund_deposit', ?, ?, ?, ?)`,
			sc.HouseholdID, sc.UserID, name, int64(amount), occurredOn, fundID)
		if err != nil {
			return fmt.Errorf("insert deposit: %w", err)
		}
		return recordAudit(ctx, tx, sc, "deposited", "fund", fundID,
			fmt.Sprintf("%s into %q", amount.Display(), name))
	})
}

// Withdraw moves money out of a fund and back into spendable cash.
func (s *Store) Withdraw(ctx context.Context, sc Scope, fundID int64, amount Cents, occurredOn string) error {
	if amount <= 0 {
		return fmt.Errorf("withdrawal must be positive")
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := fundExists(ctx, tx, sc.HouseholdID, fundID); err != nil {
			return err
		}

		balance, err := fundBalanceInTx(ctx, tx, fundID)
		if err != nil {
			return err
		}
		if amount > balance {
			return fmt.Errorf("%w: that fund holds %s", ErrInsufficientFund, balance.Display())
		}

		name, err := fundNameInTx(ctx, tx, fundID)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO transactions(household_id, user_id, kind, label, amount_cents, occurred_on, fund_id)
			VALUES (?, ?, 'fund_withdrawal', ?, ?, ?, ?)`,
			sc.HouseholdID, sc.UserID, name, int64(amount), occurredOn, fundID)
		if err != nil {
			return fmt.Errorf("insert withdrawal: %w", err)
		}
		return recordAudit(ctx, tx, sc, "withdrew", "fund", fundID,
			fmt.Sprintf("%s from %q", amount.Display(), name))
	})
}

// CloseFund returns any remaining balance to cash and marks the fund closed.
func (s *Store) CloseFund(ctx context.Context, sc Scope, fundID int64) (Cents, error) {
	var returned Cents

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := fundExists(ctx, tx, sc.HouseholdID, fundID); err != nil {
			return err
		}

		balance, err := fundBalanceInTx(ctx, tx, fundID)
		if err != nil {
			return err
		}
		name, err := fundNameInTx(ctx, tx, fundID)
		if err != nil {
			return err
		}

		if balance > 0 {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO transactions(household_id, user_id, kind, label, amount_cents, occurred_on, fund_id)
				VALUES (?, ?, 'fund_withdrawal', ?, ?, ?, ?)`,
				sc.HouseholdID, sc.UserID, "Closed: "+name, int64(balance), Today(), fundID)
			if err != nil {
				return fmt.Errorf("return fund balance: %w", err)
			}
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE funds SET closed_at = ? WHERE id = ? AND household_id = ? AND closed_at IS NULL`,
			time.Now().UTC().Format(time.RFC3339), fundID, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("close fund: %w", err)
		}
		if err := requireOneRow(res); err != nil {
			return err
		}

		returned = balance
		return recordAudit(ctx, tx, sc, "closed", "fund", fundID,
			fmt.Sprintf("%q closed, %s returned to cash", name, balance.Display()))
	})

	return returned, err
}

// EmergencyFundName is used when one is created automatically.
const EmergencyFundName = "Emergency fund"

// EmergencyFund returns the household's emergency fund, creating it on first use.
func (s *Store) EmergencyFund(ctx context.Context, sc Scope) (Fund, error) {
	f, err := s.emergencyFund(ctx, sc)
	if !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}

	// Creating it is a read-decide-write against a partial unique index
	// (idx_funds_one_emergency), so it happens in one transaction. Two
	// dashboard loads arriving together -- two tabs, or a page and its own
	// refresh -- otherwise both found no fund and the second INSERT failed the
	// constraint, turning a household's first visit into a 500.
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The other request may have created it between the read above and here.
		var existing int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM funds
			WHERE household_id = ? AND is_emergency = 1 AND closed_at IS NULL`,
			sc.HouseholdID).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read emergency fund: %w", err)
		}

		// Adopt an obviously-intended fund before creating a second.
		var adoptID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM funds
			WHERE household_id = ? AND closed_at IS NULL AND is_emergency = 0
			  AND LOWER(name) LIKE '%emergency%'
			ORDER BY id ASC LIMIT 1`, sc.HouseholdID).Scan(&adoptID)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx,
				`UPDATE funds SET is_emergency = 1 WHERE id = ? AND household_id = ?`,
				adoptID, sc.HouseholdID); err != nil {
				return fmt.Errorf("adopt emergency fund: %w", err)
			}
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO funds(household_id, user_id, name, is_emergency) VALUES(?, ?, ?, 1)`,
				sc.HouseholdID, sc.UserID, EmergencyFundName); err != nil {
				return fmt.Errorf("create emergency fund: %w", err)
			}
		default:
			return fmt.Errorf("find adoptable fund: %w", err)
		}
		return nil
	}); err != nil {
		return Fund{}, err
	}
	return s.emergencyFund(ctx, sc)
}

func (s *Store) emergencyFund(ctx context.Context, sc Scope) (Fund, error) {
	return scanFund(s.db.QueryRowContext(ctx, `
		SELECT `+fundColumns+`
		FROM funds f
		WHERE f.household_id = ? AND f.is_emergency = 1 AND f.closed_at IS NULL`, sc.HouseholdID))
}

// FundWithdrawalHistory returns the monthly total withdrawn from one fund, oldest
// first: the raw material for the Emergency Fund tab's rate-of-extraction figure.
func (s *Store) FundWithdrawalHistory(ctx context.Context, sc Scope, fundID int64) ([]MonthPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(occurred_on, 1, 7) AS m, SUM(amount_cents)
		FROM transactions
		WHERE household_id = ? AND fund_id = ? AND kind = 'fund_withdrawal'
		GROUP BY m
		ORDER BY m ASC`, sc.HouseholdID, fundID)
	if err != nil {
		return nil, fmt.Errorf("fund withdrawal history: %w", err)
	}
	defer rows.Close()

	out := []MonthPoint{}
	for rows.Next() {
		var mp MonthPoint
		var v int64
		if err := rows.Scan(&mp.Month, &v); err != nil {
			return nil, fmt.Errorf("scan withdrawal history: %w", err)
		}
		// Reusing MonthPoint: Expense carries the outflow, which is what a
		// withdrawal from savings is from the fund's point of view.
		mp.Expense = Cents(v)
		out = append(out, mp)
	}
	return out, rows.Err()
}

// DepositRate summarises how fast a fund has been filling up.
type DepositRate struct {
	Total  Cents
	Months int
}

// DepositRates returns, per fund, the total deposited and how many distinct months saw
// a deposit.
func (s *Store) DepositRates(ctx context.Context, sc Scope) (map[int64]DepositRate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fund_id,
		       SUM(amount_cents),
		       COUNT(DISTINCT substr(occurred_on, 1, 7))
		FROM transactions
		WHERE household_id = ? AND kind = 'fund_deposit' AND fund_id IS NOT NULL
		GROUP BY fund_id`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("deposit rates: %w", err)
	}
	defer rows.Close()

	out := map[int64]DepositRate{}
	for rows.Next() {
		var fundID, total int64
		var months int
		if err := rows.Scan(&fundID, &total, &months); err != nil {
			return nil, fmt.Errorf("scan deposit rate: %w", err)
		}
		out[fundID] = DepositRate{Total: Cents(total), Months: months}
	}
	return out, rows.Err()
}

// ── transaction plumbing ──────────────────────────────────────────────────────

// inTx runs fn inside a transaction, rolling back on any error.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func fundExists(ctx context.Context, tx *sql.Tx, householdID, fundID int64) error {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM funds WHERE id = ? AND household_id = ? AND closed_at IS NULL`,
		fundID, householdID).Scan(&n)
	if err != nil {
		return fmt.Errorf("verify fund: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func fundNameInTx(ctx context.Context, tx *sql.Tx, fundID int64) (string, error) {
	var name string
	if err := tx.QueryRowContext(ctx,
		`SELECT name FROM funds WHERE id = ?`, fundID).Scan(&name); err != nil {
		return "", fmt.Errorf("fund name: %w", err)
	}
	return name, nil
}

func fundBalanceInTx(ctx context.Context, tx *sql.Tx, fundID int64) (Cents, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(CASE kind
			WHEN 'fund_deposit'    THEN  amount_cents
			WHEN 'fund_withdrawal' THEN -amount_cents
			ELSE 0 END), 0)
		FROM transactions WHERE fund_id = ?`, fundID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("fund balance: %w", err)
	}
	return Cents(v), nil
}

func cashInTx(ctx context.Context, tx *sql.Tx, householdID int64) (Cents, error) {
	var v int64
	err := tx.QueryRowContext(ctx,
		`SELECT IFNULL(SUM(`+cashSignSQL+`), 0) FROM transactions WHERE household_id = ?`,
		householdID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("cash in tx: %w", err)
	}
	return Cents(v), nil
}

func scanFund(row rowScanner) (Fund, error) {
	var f Fund
	var goal, balance int64
	var emergency int
	if err := row.Scan(&f.ID, &f.Name, &goal, &f.TargetMonths, &balance,
		&f.CreatedAt, &emergency); err != nil {
		return Fund{}, err
	}
	f.Goal, f.Balance = Cents(goal), Cents(balance)
	f.IsEmergency = emergency == 1
	return f, nil
}

func cleanFundName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 40
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// ═════════════════════════════════════════════════════════════════════════════
// buckets.go
// ═════════════════════════════════════════════════════════════════════════════

// CostKind distinguishes the two sorts of recurring expense the wireframe
// describes: one that is the same every month, and one whose amount has to be
// learned from what was actually spent.
type CostKind string

const (
	// CostFixed is rent, a car payment -- the same figure each month, typed in
	// by the user.
	CostFixed CostKind = "fixed"
	// CostVariable is a water or electricity bill. Its expected amount is
	// derived from the transactions entered against it.
	CostVariable CostKind = "variable"
)

// Valid reports whether k is a known cost kind.
func (k CostKind) Valid() bool { return k == CostFixed || k == CostVariable }

// Label renders the cost kind for display.
func (k CostKind) Label() string {
	if k == CostVariable {
		return "Variable"
	}
	return "Fixed"
}

// Bucket is one recurring monthly expense.
type Bucket struct {
	ID        int64
	Name      string
	Priority  int
	CostKind  CostKind
	Fixed     Cents
	Essential bool

	// Due is what this bucket is expected to need for the month being viewed.
	Due Cents
	// Spent is what has actually been paid against it in that month.
	Spent Cents
	// Allocated is how much income has been earmarked for it.
	Allocated Cents
	// Estimate is the trailing average used when a variable bucket has no
	// activity yet in the month. Zero for fixed buckets.
	Estimate Cents

	// Low and High bracket what this bucket has historically cost: both equal Fixed for a
	// fixed bucket, and the cheapest and dearest month observed for a variable one.
	Low, High Cents
}

// Shortfall is how much of the month's requirement is still unfunded.
func (b Bucket) Shortfall() Cents {
	if b.Allocated >= b.Due {
		return 0
	}
	return b.Due - b.Allocated
}

// Funded reports whether income has been allocated to cover the whole month.
func (b Bucket) Funded() bool { return b.Due > 0 && b.Allocated >= b.Due }

// Progress is the percentage of the month's requirement that is funded.
func (b Bucket) Progress() float64 { return Ratio(b.Allocated, b.Due) }

// Status classifies the bucket for styling: funded, partially funded, or not
// funded at all.
func (b Bucket) Status() string {
	switch {
	case b.Due == 0:
		return "empty"
	case b.Allocated >= b.Due:
		return "funded"
	case b.Allocated > 0:
		return "partial"
	default:
		return "unfunded"
	}
}

// ── CRUD ──────────────────────────────────────────────────────────────────────

// NewBucket is the input for creating or editing a bucket.
type NewBucket struct {
	Name      string
	CostKind  CostKind
	Fixed     Cents
	Essential bool
}

func (n *NewBucket) normalise() error {
	n.Name = cleanFundName(n.Name)
	if n.Name == "" {
		return fmt.Errorf("give the expense a name")
	}
	if !n.CostKind.Valid() {
		n.CostKind = CostFixed
	}
	if n.CostKind == CostFixed && n.Fixed <= 0 {
		return fmt.Errorf("a fixed monthly expense needs an amount")
	}
	if n.CostKind == CostVariable {
		// A variable bucket's amount comes from its transactions, so a typed-in figure would
		// be misleading.
		n.Fixed = 0
	}
	if n.Fixed < 0 {
		return fmt.Errorf("the amount cannot be negative")
	}
	return nil
}

// CreateBucket adds a recurring expense at the bottom of the priority list.
func (s *Store) CreateBucket(ctx context.Context, sc Scope, n NewBucket) (int64, error) {
	if err := n.normalise(); err != nil {
		return 0, err
	}

	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// New buckets land last. Anything else would silently demote an expense
		// the user had already ranked.
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT IFNULL(MAX(priority), -1) + 1 FROM expense_buckets
			 WHERE household_id = ? AND archived_at IS NULL`, sc.HouseholdID).Scan(&next); err != nil {
			return fmt.Errorf("next priority: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO expense_buckets(household_id, user_id, name, priority, cost_kind, fixed_cents, essential)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sc.HouseholdID, sc.UserID, n.Name, next, string(n.CostKind), int64(n.Fixed), boolToInt(n.Essential))
		if err != nil {
			return fmt.Errorf("insert bucket: %w", err)
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// UpdateBucket edits a bucket in place.
func (s *Store) UpdateBucket(ctx context.Context, sc Scope, bucketID int64, n NewBucket) error {
	if err := n.normalise(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE expense_buckets
		SET name = ?, cost_kind = ?, fixed_cents = ?, essential = ?
		WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
		n.Name, string(n.CostKind), int64(n.Fixed), boolToInt(n.Essential), bucketID, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("update bucket: %w", err)
	}
	return requireOneRow(res)
}

// ArchiveBucket retires a bucket without deleting it.
func (s *Store) ArchiveBucket(ctx context.Context, sc Scope, bucketID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE expense_buckets SET archived_at = ?
		 WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), bucketID, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("archive bucket: %w", err)
	}
	return requireOneRow(res)
}

// MoveBucket shifts a bucket one place up or down.
func (s *Store) MoveBucket(ctx context.Context, sc Scope, bucketID int64, up bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Renumber first. Priorities drift out of sequence as buckets are archived, and
		// swapping two non-adjacent numbers would then move an item several places at once.
		if err := renumberInTx(ctx, tx, sc.HouseholdID); err != nil {
			return err
		}

		var priority int
		err := tx.QueryRowContext(ctx,
			`SELECT priority FROM expense_buckets
			 WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
			bucketID, sc.HouseholdID).Scan(&priority)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read priority: %w", err)
		}

		target := priority + 1
		if up {
			target = priority - 1
		}
		if target < 0 {
			return nil // already at the top; not an error
		}

		var otherID int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM expense_buckets
			 WHERE household_id = ? AND archived_at IS NULL AND priority = ?`,
			sc.HouseholdID, target).Scan(&otherID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already at the bottom
		}
		if err != nil {
			return fmt.Errorf("find neighbour: %w", err)
		}

		// Two updates, one transaction. A partial swap would leave both rows
		// on the same priority.
		if _, err := tx.ExecContext(ctx,
			`UPDATE expense_buckets SET priority = ? WHERE id = ?`, target, bucketID); err != nil {
			return fmt.Errorf("move bucket: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE expense_buckets SET priority = ? WHERE id = ?`, priority, otherID); err != nil {
			return fmt.Errorf("move neighbour: %w", err)
		}
		return nil
	})
}

// renumberInTx compacts priorities to 0..n-1 preserving the current order.
func renumberInTx(ctx context.Context, tx *sql.Tx, householdID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM expense_buckets
		 WHERE household_id = ? AND archived_at IS NULL
		 ORDER BY priority ASC, id ASC`, householdID)
	if err != nil {
		return fmt.Errorf("renumber read: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE expense_buckets SET priority = ? WHERE id = ?`, i, id); err != nil {
			return fmt.Errorf("renumber write: %w", err)
		}
	}
	return nil
}

// ── reading with month context ────────────────────────────────────────────────

// Buckets returns the user's active buckets in priority order, each carrying
// its requirement, actual spend and funding for the given month.
func (s *Store) Buckets(ctx context.Context, sc Scope, month string) ([]Bucket, error) {
	if month == "" {
		month = Today()[:7]
	}
	start, end, err := monthRange(month)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, bucketHistoryCTE+`
		SELECT b.id, b.name, b.priority, b.cost_kind, b.fixed_cents, b.essential,
		       IFNULL((
		           SELECT SUM(t.amount_cents) FROM transactions t
		           WHERE t.bucket_id = b.id AND t.kind = 'expense'
		             AND t.occurred_on >= ? AND t.occurred_on < ?
		       ), 0) AS spent,
		       IFNULL((
		           SELECT SUM(a.amount_cents) FROM allocations a
		           WHERE a.bucket_id = b.id AND a.month = ?
		       ), 0) AS allocated,
		       IFNULL(h.estimate, 0), IFNULL(h.low, 0), IFNULL(h.high, 0)
		FROM expense_buckets b
		LEFT JOIN history h ON h.bucket_id = b.id
		WHERE b.household_id = ? AND b.archived_at IS NULL
		ORDER BY b.priority ASC, b.id ASC`,
		sc.HouseholdID, start, start, end, month, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	defer rows.Close()

	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		var kind string
		var fixed, spent, allocated, low, high int64
		var essential int
		var estimate float64
		if err := rows.Scan(&b.ID, &b.Name, &b.Priority, &kind, &fixed, &essential,
			&spent, &allocated, &estimate, &low, &high); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		b.CostKind = CostKind(kind)
		b.Fixed = Cents(fixed)
		b.Essential = essential == 1
		b.Spent = Cents(spent)
		b.Allocated = Cents(allocated)
		// AVG returns a float; round rather than truncate so an estimate of
		// 1999.6 cents does not present as 19.99 when it should be 20.00.
		b.Estimate = Cents(int64(estimate + 0.5))
		b.Due = bucketDue(b)
		b.Low, b.High = bucketRange(b, Cents(low), Cents(high))
		out = append(out, b)
	}
	return out, rows.Err()
}

// bucketHistoryCTE summarises each bucket's trailing six months of activity
// before a given date: the mean, cheapest and dearest month, which are the
// estimate and range for a variable bucket. It takes two arguments, the
// household id and the start date, and is prefixed to the queries that need it
// so the figure the waterfall funds and the figure the dashboard shows come from
// the same SQL. Months are ranked with a window function so the six-month
// window is applied per bucket in a single pass over the table.
const bucketHistoryCTE = `
	WITH months AS (
		SELECT bucket_id, substr(occurred_on, 1, 7) AS month, SUM(amount_cents) AS total
		FROM transactions
		WHERE household_id = ? AND kind = 'expense' AND bucket_id IS NOT NULL
		  AND occurred_on < ?
		GROUP BY bucket_id, month
	), recent AS (
		SELECT bucket_id, total,
		       ROW_NUMBER() OVER (PARTITION BY bucket_id ORDER BY month DESC) AS rank
		FROM months
	), history AS (
		SELECT bucket_id, AVG(total) AS estimate, MIN(total) AS low, MAX(total) AS high
		FROM recent WHERE rank <= 6
		GROUP BY bucket_id
	)`

// bucketDue computes what a bucket needs for the month, as a plain function so the
// allocation code and the display code cannot disagree about the number.
func bucketDue(b Bucket) Cents {
	if b.CostKind == CostFixed {
		return b.Fixed
	}
	// A variable bucket costs whatever was actually spent on it this month; if
	// nothing has been entered yet, fall back to the trailing average so the
	// month can still be budgeted for in advance.
	if b.Spent > 0 {
		return b.Spent
	}
	return b.Estimate
}

// bucketRange brackets what a bucket costs. A fixed bucket has no range: pretending
// otherwise would invent uncertainty.
func bucketRange(b Bucket, low, high Cents) (Cents, Cents) {
	if b.CostKind == CostFixed {
		return b.Fixed, b.Fixed
	}

	if low == 0 && high == 0 {
		// No history at all: the estimate is the only figure available, so the
		// range collapses onto it.
		return b.Estimate, b.Estimate
	}
	if b.Spent > high {
		high = b.Spent
	}
	if b.Spent > 0 && b.Spent < low {
		low = b.Spent
	}
	return low, high
}

// BucketOptions lists active buckets for a form's select element.
func (s *Store) BucketOptions(ctx context.Context, sc Scope) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, priority, cost_kind, fixed_cents, essential
		 FROM expense_buckets WHERE household_id = ? AND archived_at IS NULL
		 ORDER BY priority ASC, id ASC`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("bucket options: %w", err)
	}
	defer rows.Close()

	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		var kind string
		var fixed int64
		var essential int
		if err := rows.Scan(&b.ID, &b.Name, &b.Priority, &kind, &fixed, &essential); err != nil {
			return nil, err
		}
		b.CostKind, b.Fixed, b.Essential = CostKind(kind), Cents(fixed), essential == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

// EssentialCost sums the monthly requirement of every bucket tagged essential,
// which is how the emergency fund target is sized.
//
// It takes the month's buckets rather than fetching them: every caller is
// already holding them, and Buckets is the most expensive read in the app.
func EssentialCost(buckets []Bucket) Cents {
	var total Cents
	for _, b := range buckets {
		if b.Essential {
			total += b.Due
		}
	}
	return total
}

// ═════════════════════════════════════════════════════════════════════════════
// allocations.go
// ═════════════════════════════════════════════════════════════════════════════

// Allocation is one slice of an income earmarked for one bucket.
type Allocation struct {
	ID         int64
	IncomeID   int64
	BucketID   int64
	BucketName string
	Month      string
	Amount     Cents
	IncomeName string
	OccurredOn string
}

// AllocationSummary is the month's funding picture.
type AllocationSummary struct {
	Month      string
	Income     Cents // income received in the month
	Required   Cents // sum of every bucket's requirement
	Allocated  Cents // how much of the income has been earmarked
	Unassigned Cents // income left over after every bucket is funded
	Shortfall  Cents // requirement that no income covers
}

// FullyFunded reports whether every recurring expense is covered.
func (a AllocationSummary) FullyFunded() bool {
	return a.Required > 0 && a.Shortfall == 0
}

// Progress is the percentage of the month's requirement that is funded.
func (a AllocationSummary) Progress() float64 { return Ratio(a.Allocated, a.Required) }

// Reallocate recomputes every allocation for one month from scratch.
func (s *Store) Reallocate(ctx context.Context, sc Scope, month string) error {
	if month == "" {
		month = Today()[:7]
	}
	start, end, err := monthRange(month)
	if err != nil {
		return err
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		// 1. Clear the month. Scoped to user and month so one user's
		//    recalculation cannot touch another's, or another month's.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM allocations WHERE household_id = ? AND month = ?`, sc.HouseholdID, month); err != nil {
			return fmt.Errorf("clear allocations: %w", err)
		}

		// 2. Read the buckets in priority order, with the requirement for each.
		type target struct {
			id       int64
			due      Cents
			assigned Cents
		}
		rows, err := tx.QueryContext(ctx, bucketHistoryCTE+`
			SELECT b.id, b.cost_kind, b.fixed_cents,
			       IFNULL((
			           SELECT SUM(t.amount_cents) FROM transactions t
			           WHERE t.bucket_id = b.id AND t.kind = 'expense'
			             AND t.occurred_on >= ? AND t.occurred_on < ?
			       ), 0) AS spent,
			       IFNULL(h.estimate, 0) AS estimate
			FROM expense_buckets b
			LEFT JOIN history h ON h.bucket_id = b.id
			WHERE b.household_id = ? AND b.archived_at IS NULL
			ORDER BY b.priority ASC, b.id ASC`,
			sc.HouseholdID, start, start, end, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("read buckets for allocation: %w", err)
		}

		var targets []target
		for rows.Next() {
			var b Bucket
			var kind string
			var fixed, spent int64
			var estimate float64
			if err := rows.Scan(&b.ID, &kind, &fixed, &spent, &estimate); err != nil {
				rows.Close()
				return fmt.Errorf("scan bucket for allocation: %w", err)
			}
			b.CostKind, b.Fixed, b.Spent = CostKind(kind), Cents(fixed), Cents(spent)
			b.Estimate = Cents(int64(estimate + 0.5))
			if due := bucketDue(b); due > 0 {
				targets = append(targets, target{id: b.ID, due: due})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(targets) == 0 {
			return nil
		}

		// 3. Replay the month's income oldest first, so an earlier payday funds
		//    the top of the list even if a later one is larger.
		incRows, err := tx.QueryContext(ctx, `
			SELECT id, amount_cents FROM transactions
			WHERE household_id = ? AND kind = 'income'
			  AND occurred_on >= ? AND occurred_on < ?
			ORDER BY occurred_on ASC, id ASC`, sc.HouseholdID, start, end)
		if err != nil {
			return fmt.Errorf("read income for allocation: %w", err)
		}
		type income struct {
			id     int64
			amount Cents
		}
		var incomes []income
		for incRows.Next() {
			var in income
			var amt int64
			if err := incRows.Scan(&in.id, &amt); err != nil {
				incRows.Close()
				return fmt.Errorf("scan income: %w", err)
			}
			in.amount = Cents(amt)
			incomes = append(incomes, in)
		}
		incRows.Close()
		if err := incRows.Err(); err != nil {
			return err
		}

		// 4. The waterfall. For each income, walk the priority list and pour
		//    into each bucket until it is full or the money runs out.
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO allocations(household_id, user_id, income_id, bucket_id, month, amount_cents)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare allocation insert: %w", err)
		}
		defer stmt.Close()

		for _, in := range incomes {
			remaining := in.amount
			for i := range targets {
				if remaining <= 0 {
					break
				}
				need := targets[i].due - targets[i].assigned
				if need <= 0 {
					continue
				}
				take := need
				if remaining < take {
					take = remaining
				}
				if _, err := stmt.ExecContext(ctx, sc.HouseholdID, sc.UserID, in.id, targets[i].id, month, int64(take)); err != nil {
					return fmt.Errorf("insert allocation: %w", err)
				}
				targets[i].assigned += take
				remaining -= take
			}
			// Whatever is left over stays unassigned; it is not an error, and
			// it is reported to the user as money free to spend or save.
		}
		return nil
	})
}

// ReallocateMonthOf recomputes the month containing the given date.
func (s *Store) ReallocateMonthOf(ctx context.Context, sc Scope, date string) error {
	if len(date) < 7 {
		return s.Reallocate(ctx, sc, Today()[:7])
	}
	return s.Reallocate(ctx, sc, date[:7])
}

// AllocationsFor returns the month's summary. The month's buckets are passed
// in rather than re-read: only their Due totals are wanted here, and every
// caller has just fetched them.
func (s *Store) AllocationsFor(ctx context.Context, sc Scope, month string, buckets []Bucket) (AllocationSummary, error) {
	if month == "" {
		month = Today()[:7]
	}
	start, end, err := monthRange(month)
	if err != nil {
		return AllocationSummary{}, err
	}

	sum := AllocationSummary{Month: month}

	if err := s.db.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(amount_cents), 0) FROM transactions
		WHERE household_id = ? AND kind = 'income'
		  AND occurred_on >= ? AND occurred_on < ?`,
		sc.HouseholdID, start, end).Scan(&sum.Income); err != nil {
		return AllocationSummary{}, fmt.Errorf("allocation income: %w", err)
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(amount_cents), 0) FROM allocations
		WHERE household_id = ? AND month = ?`, sc.HouseholdID, month).Scan(&sum.Allocated); err != nil {
		return AllocationSummary{}, fmt.Errorf("allocation total: %w", err)
	}

	for _, b := range buckets {
		sum.Required += b.Due
	}

	sum.Unassigned = sum.Income - sum.Allocated
	if sum.Unassigned < 0 {
		sum.Unassigned = 0
	}
	sum.Shortfall = sum.Required - sum.Allocated
	if sum.Shortfall < 0 {
		sum.Shortfall = 0
	}
	return sum, nil
}

// AllocationsForBucket lists which income funded one bucket in a month, so the
// user can see where the money for their rent actually came from.
func (s *Store) AllocationsForBucket(ctx context.Context, sc Scope, bucketID int64, month string) ([]Allocation, error) {
	if month == "" {
		month = Today()[:7]
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.income_id, a.bucket_id, b.name, a.month, a.amount_cents,
		       t.label, t.occurred_on
		FROM allocations a
		JOIN expense_buckets b ON b.id = a.bucket_id
		JOIN transactions   t ON t.id = a.income_id
		WHERE a.household_id = ? AND a.bucket_id = ? AND a.month = ?
		ORDER BY t.occurred_on ASC, a.id ASC`, sc.HouseholdID, bucketID, month)
	if err != nil {
		return nil, fmt.Errorf("bucket allocations: %w", err)
	}
	defer rows.Close()

	out := []Allocation{}
	for rows.Next() {
		var a Allocation
		var amount int64
		if err := rows.Scan(&a.ID, &a.IncomeID, &a.BucketID, &a.BucketName,
			&a.Month, &amount, &a.IncomeName, &a.OccurredOn); err != nil {
			return nil, fmt.Errorf("scan allocation: %w", err)
		}
		a.Amount = Cents(amount)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ═════════════════════════════════════════════════════════════════════════════
// lineitems.go
// ═════════════════════════════════════════════════════════════════════════════

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

		var sum Cents
		for _, it := range items {
			if it.Amount <= 0 {
				return fmt.Errorf("every line item needs an amount greater than zero")
			}
			sum += it.Amount
		}
		if sum != total {
			return fmt.Errorf("the line items add up to %s but the transaction is %s",
				sum.Display(), total.Display())
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
	})
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

// LineItemCounts returns how many lines each of the given transactions has, so
// a list page can show an expander without a query per row.
func (s *Store) LineItemCounts(ctx context.Context, sc Scope, txIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(txIDs) == 0 {
		return out, nil
	}

	// Build the IN list from placeholders rather than by interpolating the ids.
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
		SELECT li.transaction_id, COUNT(*)
		FROM line_items li
		JOIN transactions t ON t.id = li.transaction_id
		WHERE t.household_id = ? AND li.transaction_id IN (`+string(ph)+`)
		GROUP BY li.transaction_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("line item counts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CategoryBreakdown totals spending by category, using line-item categories where a
// transaction has them and its own label where it does not.
func (s *Store) CategoryBreakdown(ctx context.Context, sc Scope, month string) ([]LabelTotal, error) {
	clause, span, err := monthClause(month, "t.")
	if err != nil {
		return nil, err
	}
	where := `t.household_id = ? AND t.kind = 'expense'` + clause
	args := append([]any{sc.HouseholdID}, span...)
	// The argument list is consumed twice, once per half of the UNION ALL.
	args = append(args, args...)

	rows, err := s.db.QueryContext(ctx, `
		SELECT grp, SUM(amount) AS total FROM (
			-- transactions that have been broken into lines
			SELECT CASE WHEN TRIM(li.category) = '' THEN 'Uncategorised'
			            ELSE TRIM(li.category) END AS grp,
			       li.amount_cents AS amount
			FROM line_items li
			JOIN transactions t ON t.id = li.transaction_id
			WHERE `+where+`

			UNION ALL

			-- and those that stand alone
			SELECT CASE WHEN TRIM(t.label) = '' THEN 'Uncategorised'
			            ELSE TRIM(t.label) END AS grp,
			       t.amount_cents AS amount
			FROM transactions t
			WHERE `+where+`
			  AND NOT EXISTS (SELECT 1 FROM line_items li2 WHERE li2.transaction_id = t.id)
		)
		GROUP BY grp
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

// ═════════════════════════════════════════════════════════════════════════════
// budgets.go
// ═════════════════════════════════════════════════════════════════════════════

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

	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.category, b.limit_cents,
		       IFNULL((
		           SELECT SUM(t.amount_cents)
		           FROM transactions t
		           WHERE t.household_id = b.household_id
		             AND t.kind = 'expense'
		             AND LOWER(TRIM(t.label)) = LOWER(TRIM(b.category))
		             AND t.occurred_on >= ? AND t.occurred_on < ?
		       ), 0) AS spent
		FROM budgets b
		WHERE b.household_id = ?
		ORDER BY b.category COLLATE NOCASE ASC`, start, end, sc.HouseholdID)
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

// ═════════════════════════════════════════════════════════════════════════════
// users.go
// ═════════════════════════════════════════════════════════════════════════════

// User is an account. It never carries the password hash.
type User struct {
	ID          int64
	Email       string
	DisplayName string
}

// Name is what the UI greets the user with.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	// Fall back to the part before the @, so the header reads "kushith" rather
	// than the full address.
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return u.Email
}

// NormalizeEmail lowercases and trims a login identifier so that
// "Bob@X.com " and "bob@x.com" cannot become two accounts.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// CreateUser inserts a new account, with display_name derived from the address because
// signup asks only for an email and a password.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (int64, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return 0, fmt.Errorf("email is required")
	}

	display := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		display = email[:i]
	}

	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM users
			WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE`,
			email, email).Scan(&n); err != nil {
			return fmt.Errorf("check existing account: %w", err)
		}
		if n > 0 {
			return ErrEmailTaken
		}

		// username is written as well as email: a vestige of the old schema that migration 3
		// deliberately did not drop, because rebuilding the table would have cascaded and
		// deleted every transaction. It is NOT NULL UNIQUE, so it still needs a value.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users(username, email, display_name, password_hash) VALUES(?, ?, ?, ?)`,
			email, email, display, passwordHash)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}

		// Every account owns a personal household from the moment it exists, in the same
		// transaction as the insert: a user committed without one could sign in and have
		// nowhere to record anything, and every page would have to cope with that state.
		_, err = createPersonalHousehold(ctx, tx, id, display)
		return err
	})

	return id, err
}

// CredentialsFor looks up an account and returns its password hash, matching exactly
// first and case-insensitively second.
func (s *Store) CredentialsFor(ctx context.Context, email string) (User, string, error) {
	typed := strings.TrimSpace(email)
	u, hash, err := s.credentials(ctx, typed, "")
	if errors.Is(err, ErrNotFound) {
		// Case-insensitive second. LIMIT by lowest id keeps the result
		// deterministic if an ambiguous legacy pair is somehow reached from here.
		u, hash, err = s.credentials(ctx, NormalizeEmail(typed), " COLLATE NOCASE")
	}
	return u, hash, err
}

// credentials looks an account up by email or legacy username, with the
// collation given ("" for exact, " COLLATE NOCASE" for case-insensitive).
func (s *Store) credentials(ctx context.Context, ident, collate string) (User, string, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), password_hash
		FROM users
		WHERE email = ?`+collate+` OR username = ?`+collate+`
		ORDER BY id ASC
		LIMIT 1`, ident, ident).Scan(&u.ID, &u.Email, &u.DisplayName, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("select user: %w", err)
	}
	return u, hash, nil
}

// EmailExists reports whether an address already has an account, which is how the
// combined login and signup form decides which of the two the user is doing.
func (s *Store) EmailExists(ctx context.Context, email string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users
		WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE`,
		NormalizeEmail(email), NormalizeEmail(email)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("email exists: %w", err)
	}
	return n > 0, nil
}

// ═════════════════════════════════════════════════════════════════════════════
// sessions
// ═════════════════════════════════════════════════════════════════════════════

// A login is a row here, not just a signed cookie.

// after and ago render a duration as a SQLite datetime modifier, "+86400 seconds"
// and "-86400 seconds", so every expiry is computed by SQLite in the same clock
// the later comparison uses -- and derived from the Go constant, so the two
// cannot disagree.
func after(d time.Duration) string { return fmt.Sprintf("+%d seconds", int64(d.Seconds())) }
func ago(d time.Duration) string   { return fmt.Sprintf("-%d seconds", int64(d.Seconds())) }

const (
	// SessionTTL is how long a login lasts from the moment it is created, however active
	// it is.
	SessionTTL = 30 * 24 * time.Hour

	// SessionIdleTTL is how long a login may go untouched before it counts as abandoned.
	SessionIdleTTL = 14 * 24 * time.Hour

	// sessionTouchAfter is how stale last_seen_at must be before a request updates it.
	sessionTouchAfter = time.Minute
)

// Session is one active login, as shown on the device list.
type Session struct {
	ID         string
	UserID     int64
	CreatedAt  string
	LastSeenAt string
	ExpiresAt  string
	UserAgent  string

	// Current marks the session making the request, so the UI can label it and
	// not offer to sign it out alongside the others.
	Current bool
}

// DeviceName turns the browser's self-description into something recognisable -- Chrome
// on Windows rather than 120 characters of Mozilla/5.0 -- so a login that is not yours
// can be spotted.
func (s Session) DeviceName() string {
	ua := s.UserAgent
	if strings.TrimSpace(ua) == "" {
		return "Unknown device"
	}

	browser := ""
	switch {
	case strings.Contains(ua, "Edg"):
		browser = "Edge"
	case strings.Contains(ua, "OPR"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "SamsungBrowser"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "Firefox"), strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "CriOS"), strings.Contains(ua, "Chrome"),
		strings.Contains(ua, "Chromium"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari"):
		browser = "Safari"
	}

	// iPhone and iPad are checked before Mac, because their user agents contain
	// "like Mac OS X"; Android is checked before Linux for the same reason.
	device := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		device = "iPhone"
	case strings.Contains(ua, "iPad"):
		device = "iPad"
	case strings.Contains(ua, "Android"):
		device = "Android"
	case strings.Contains(ua, "Windows"):
		device = "Windows"
	case strings.Contains(ua, "CrOS"):
		device = "ChromeOS"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		device = "Mac"
	case strings.Contains(ua, "Linux"):
		device = "Linux"
	}

	switch {
	case browser != "" && device != "":
		return browser + " on " + device
	case browser != "":
		return browser
	case device != "":
		return device
	}
	return "Unknown device"
}

// newSessionID returns a fresh 256-bit random token.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanUserAgent trims a browser's self-description to something loggable.
func cleanUserAgent(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// CreateSession issues a login and returns its token.
func (s *Store) CreateSession(ctx context.Context, userID int64, userAgent string) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	ttl := after(SessionTTL)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, user_id, expires_at, user_agent)
		VALUES (?, ?, datetime('now', ?), ?)`,
		id, userID, ttl, cleanUserAgent(userAgent)); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// SessionUser resolves a session token to the account it belongs to.
func (s *Store) SessionUser(ctx context.Context, id string) (User, error) {
	if id == "" {
		return User{}, ErrNotFound
	}

	idle := ago(SessionIdleTTL)

	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, '')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = ?
		  AND s.expires_at   > datetime('now')
		  AND s.last_seen_at > datetime('now', ?)`,
		id, idle,
	).Scan(&u.ID, &u.Email, &u.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve session: %w", err)
	}

	stale := ago(sessionTouchAfter)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = datetime('now')
		WHERE id = ? AND last_seen_at < datetime('now', ?)`, id, stale); err != nil {
		// Not fatal: the caller is authenticated either way, and failing the request over a
		// bookkeeping write would be a poor trade.
		return u, nil
	}
	return u, nil
}

// Sessions lists a user's live logins, most recently active first.
func (s *Store) Sessions(ctx context.Context, userID int64, current string) ([]Session, error) {
	idle := ago(SessionIdleTTL)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, created_at, last_seen_at, expires_at, user_agent
		FROM sessions
		WHERE user_id = ?
		  AND expires_at   > datetime('now')
		  AND last_seen_at > datetime('now', ?)
		ORDER BY last_seen_at DESC`, userID, idle)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.UserID, &v.CreatedAt, &v.LastSeenAt,
			&v.ExpiresAt, &v.UserAgent); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		v.Current = v.ID == current
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteSession revokes one login. user_id is part of the WHERE clause so a guessed
// token cannot be used to sign somebody else out.
func (s *Store) DeleteSession(ctx context.Context, userID int64, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserSessions revokes every login for an account.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOtherSessions revokes every login for an account except the one given.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keep)
	if err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeExpiredSessions removes rows no login can use.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	idle := ago(SessionIdleTTL)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE expires_at <= datetime('now')
		   OR last_seen_at <= datetime('now', ?)`, idle)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ── password resets ───────────────────────────────────────────────────────────

// ResetTTL is how long a password reset link works.
const ResetTTL = time.Hour

// CreateReset issues a single-use password reset token, deleting every other
// outstanding one for the account: only the most recent link should work, and the
// owner requesting their own invalidates one an attacker asked for.
func (s *Store) CreateReset(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionID() // same generator: 256 bits of crypto/rand
	if err != nil {
		return "", err
	}
	ttl := after(ResetTTL)

	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clear old resets: %w", err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO password_resets(token, user_id, expires_at)
			 VALUES(?, ?, datetime('now', ?))`, token, userID, ttl)
		if err != nil {
			return fmt.Errorf("create reset: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ResetUser resolves a token to the account it belongs to, without consuming it.
func (s *Store) ResetUser(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, '')
		FROM password_resets p
		JOIN users u ON u.id = p.user_id
		WHERE p.token = ? AND p.used_at IS NULL AND p.expires_at > datetime('now')`,
		token,
	).Scan(&u.ID, &u.Email, &u.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve reset token: %w", err)
	}
	return u, nil
}

// ConsumeReset sets a new password hash and burns the token.
func (s *Store) ConsumeReset(ctx context.Context, token, passwordHash string) (int64, error) {
	var userID int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE password_resets SET used_at = datetime('now')
			WHERE token = ? AND used_at IS NULL AND expires_at > datetime('now')
			RETURNING user_id`, token).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("consume reset token: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ? WHERE id = ?`,
			passwordHash, userID); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ? AND token <> ?`,
			userID, token); err != nil {
			return fmt.Errorf("clear other resets: %w", err)
		}
		return nil
	})
	return userID, err
}

// ChangePassword replaces the hash for a signed-in user and signs out their other
// devices.
func (s *Store) ChangePassword(ctx context.Context, userID int64, passwordHash, keepSession string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
		if err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ? AND id <> ?`,
			userID, keepSession); err != nil {
			return fmt.Errorf("revoke other sessions: %w", err)
		}
		// A pending reset link is stale the moment the password changes.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clear resets: %w", err)
		}
		return nil
	})
}

// PurgeExpiredResets removes tokens that can no longer be used.
func (s *Store) PurgeExpiredResets(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM password_resets
		 WHERE expires_at <= datetime('now') OR used_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("purge resets: %w", err)
	}
	return res.RowsAffected()
}

// ── one-time form tokens ──────────────────────────────────────────────────────

// FormTokenTTL is how long an unused form token stays valid: long enough to fill in a
// form slowly and be interrupted, short enough not to accumulate forgotten tabs.
const FormTokenTTL = 12 * time.Hour

// NewFormToken issues a token to embed in a form that must not be submitted twice.
func (s *Store) NewFormToken(ctx context.Context, userID int64, purpose string) (string, error) {
	token, err := newSessionID()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO form_tokens(token, user_id, purpose) VALUES(?, ?, ?)`,
		token, userID, purpose)
	if err != nil {
		return "", fmt.Errorf("issue form token: %w", err)
	}
	return token, nil
}

// ConsumeFormToken spends a token, reporting whether this caller got it.
func (s *Store) ConsumeFormToken(ctx context.Context, userID int64, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	age := ago(FormTokenTTL)

	var got string
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM form_tokens
		WHERE token = ? AND user_id = ? AND created_at > datetime('now', ?)
		RETURNING token`, token, userID, age).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume form token: %w", err)
	}
	return true, nil
}

// PurgeOldFormTokens drops tokens for forms nobody ever submitted.
func (s *Store) PurgeOldFormTokens(ctx context.Context) (int64, error) {
	age := ago(FormTokenTTL)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM form_tokens WHERE created_at <= datetime('now', ?)`, age)
	if err != nil {
		return 0, fmt.Errorf("purge form tokens: %w", err)
	}
	return res.RowsAffected()
}

// ── audit log ─────────────────────────────────────────────────────────────────

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID        int64
	Actor     string // display name or email; "a removed account" if the user is gone
	Action    string // created, edited, deleted, deposited, withdrew, ...
	Entity    string // transaction, fund, member, invitation, budget
	EntityID  *int64
	Summary   string
	CreatedAt string
}

// recordAudit writes one entry inside the caller's transaction, deliberately not its
// own.
func recordAudit(ctx context.Context, tx *sql.Tx, sc Scope,
	action, entity string, entityID int64, summary string) error {
	var id any
	if entityID != 0 {
		id = entityID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log(household_id, user_id, action, entity, entity_id, summary)
		VALUES(?, ?, ?, ?, ?, ?)`,
		sc.HouseholdID, sc.UserID, action, entity, id, summary)
	if err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// AuditLog returns a household's recent history, newest first.
func (s *Store) AuditLog(ctx context.Context, sc Scope, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, u.username)),
		       a.action, a.entity, a.entity_id, a.summary, a.created_at
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.user_id
		WHERE a.household_id = ?
		ORDER BY a.id DESC
		LIMIT ?`, sc.HouseholdID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var actor sql.NullString
		if err := rows.Scan(&e.ID, &actor, &e.Action, &e.Entity,
			&e.EntityID, &e.Summary, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		// The account was deleted. The record of what it did survives, which is
		// the point of ON DELETE SET NULL on that column.
		e.Actor = "a removed account"
		if actor.Valid && actor.String != "" {
			e.Actor = actor.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── login rate limiting ───────────────────────────────────────────────────────

// RateWindow and RateMaxTries define the login limit.
//
// RateBurstTries is the separate ceiling for a whole-IP counter. It is several
// times RateMaxTries on purpose: ten failures against one address is somebody
// guessing at that account, but sixty failures from one address spread over
// many accounts is a password spray, and a shared office or campus network has
// to be able to mistype passwords all morning without the building losing
// access.
const (
	RateWindow     = 10 * time.Minute
	RateMaxTries   = 10
	RateBurstTries = 60
)

// RateRetryIn reports how long a key must wait, or zero if it may try now.
func (s *Store) RateRetryIn(ctx context.Context, key string) (time.Duration, error) {
	return s.RateRetryInMax(ctx, key, RateMaxTries)
}

// RateRetryInMax is RateRetryIn against a caller-chosen budget, for counters
// that are not one-per-account.
func (s *Store) RateRetryInMax(ctx context.Context, key string, maxTries int64) (time.Duration, error) {
	window := ago(RateWindow)
	ahead := after(RateWindow)

	// Seconds remaining are computed in SQL rather than by parsing the timestamp in Go.
	var failures, remaining int64
	err := s.db.QueryRowContext(ctx, `
		SELECT failures,
		       strftime('%s', window_start, ?) - strftime('%s', 'now')
		FROM login_attempts
		WHERE key = ? AND window_start > datetime('now', ?)`,
		ahead, key, window).Scan(&failures, &remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read login attempts: %w", err)
	}
	if failures < maxTries {
		return 0, nil
	}
	if remaining <= 0 {
		// The window has just lapsed between the WHERE clause and this line.
		return 0, nil
	}
	return time.Duration(remaining) * time.Second, nil
}

// RateFail records a failed attempt, starting a new window when the old one has aged
// out.
func (s *Store) RateFail(ctx context.Context, key string) error {
	window := ago(RateWindow)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO login_attempts(key, failures, window_start)
		VALUES(?, 1, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET
			failures = CASE
				WHEN login_attempts.window_start > datetime('now', ?) THEN login_attempts.failures + 1
				ELSE 1
			END,
			window_start = CASE
				WHEN login_attempts.window_start > datetime('now', ?) THEN login_attempts.window_start
				ELSE datetime('now')
			END`, key, window, window)
	if err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	return nil
}

// RateReset clears the counter after a successful sign-in.
func (s *Store) RateReset(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("reset login attempts: %w", err)
	}
	return nil
}

// PurgeOldAttempts drops windows that have expired, so the table does not grow
// with one row per address that ever mistyped a password.
func (s *Store) PurgeOldAttempts(ctx context.Context) (int64, error) {
	window := ago(RateWindow)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_attempts WHERE window_start <= datetime('now', ?)`, window)
	if err != nil {
		return 0, fmt.Errorf("purge login attempts: %w", err)
	}
	return res.RowsAffected()
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique")
}

// ═════════════════════════════════════════════════════════════════════════════
// households.go
// ═════════════════════════════════════════════════════════════════════════════

// A household is the thing that owns money, and a Role is what a person may do to it.

// ── roles ─────────────────────────────────────────────────────────────────────

// Role is a member's authority within one household.
type Role string

const (
	// RoleOwner administers the household: members, invitations, renaming,
	// deletion, and moving money between savings funds.
	RoleOwner Role = "owner"

	// RoleEditor records the household's day-to-day money -- income, expenses,
	// recurring bills, category budgets -- but does not administer it and does
	// not move money into or out of savings funds.
	RoleEditor Role = "editor"

	// RoleViewer reads everything and changes nothing.
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a role this application recognises.
func (r Role) Valid() bool {
	return r == RoleOwner || r == RoleEditor || r == RoleViewer
}

// Label is the human name for a role.
func (r Role) Label() string {
	switch r {
	case RoleOwner:
		return "Owner"
	case RoleEditor:
		return "Editor"
	case RoleViewer:
		return "Viewer"
	}
	return "No access"
}

// Explain is the one-line description shown beside the role in the UI.
func (r Role) Explain() string {
	switch r {
	case RoleOwner:
		return "Full control, including members and savings funds."
	case RoleEditor:
		return "Can add and edit income and expenses, but not move savings."
	case RoleViewer:
		return "Can see everything. Cannot change anything."
	}
	return ""
}

// CanEditEntries covers income, expenses, recurring expense buckets, category
// budgets and reallocation -- everything that records what the household
// actually earned and spent.
func (r Role) CanEditEntries() bool { return r == RoleOwner || r == RoleEditor }

// CanMoveFunds covers depositing to, withdrawing from and closing a savings fund, and
// is owner-only.
func (r Role) CanMoveFunds() bool { return r == RoleOwner }

// CanManageMembers covers inviting, removing, and changing roles.
func (r Role) CanManageMembers() bool { return r == RoleOwner }

// CanManageHousehold covers renaming and deleting the household itself.
func (r Role) CanManageHousehold() bool { return r == RoleOwner }

// ── errors ────────────────────────────────────────────────────────────────────

var (
	// ErrNotMember means the caller is not in the household they asked about.
	ErrNotMember = errors.New("not a member of that household")

	// ErrForbidden means the caller is a member but their role does not permit the action.
	ErrForbidden = errors.New("your role does not allow that")

	// ErrLastOwner blocks removing or demoting the only owner, which would
	// leave a household nobody could administer.
	ErrLastOwner = errors.New("a household must always have at least one owner")

	// ErrAlreadyMember is returned instead of a UNIQUE violation when inviting
	// somebody who has already joined.
	ErrAlreadyMember = errors.New("that person is already a member")

	// ErrInviteOpen means an unanswered invitation for that address exists.
	ErrInviteOpen = errors.New("that address already has an invitation pending")

	// ErrPersonalHousehold blocks administering a personal household as though
	// it were shared: it cannot be renamed away, left, or deleted, because it is
	// where the user's own data lives.
	ErrPersonalHousehold = errors.New("your personal budget cannot be shared or removed")
)

// ── types ─────────────────────────────────────────────────────────────────────

// Household is one budget that one or more people work on.
type Household struct {
	ID       int64
	Name     string
	Personal bool
	Members  int
}

// Membership is a household together with the caller's authority in it.
type Membership struct {
	Household
	Role Role
}

// Member is one person in a household, as shown on the settings page.
type Member struct {
	UserID      int64
	Email       string
	DisplayName string
	Role        Role
	JoinedAt    string
	IsSelf      bool
}

// Name is what to show for a member, preferring the display name.
func (m Member) Name() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	if i := strings.IndexByte(m.Email, '@'); i > 0 {
		return m.Email[:i]
	}
	return m.Email
}

// Invite is an unanswered invitation.
type Invite struct {
	ID            int64
	HouseholdID   int64
	HouseholdName string
	Email         string
	Role          Role
	InvitedBy     string
	CreatedAt     string

	// ExpiresAt is when the invitation stops being acceptable, and Expired says whether
	// that has happened.
	ExpiresAt string
	Expired   bool
}

// ── creating households ───────────────────────────────────────────────────────

// createPersonalHousehold inserts a user's own household and makes them its owner,
// inside the caller's transaction: a user committed without one could log in and have
// nowhere to put anything. Called by CreateUser, and by ActiveHousehold as a repair.
func createPersonalHousehold(ctx context.Context, tx *sql.Tx, userID int64, display string) (int64, error) {
	name := strings.TrimSpace(display)
	if name == "" {
		name = "My"
	}
	name += "'s budget"

	res, err := tx.ExecContext(ctx,
		`INSERT INTO households(name, personal_for) VALUES(?, ?)`, name, userID)
	if err != nil {
		return 0, fmt.Errorf("create personal household: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`,
		id, userID); err != nil {
		return 0, fmt.Errorf("create owner membership: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET active_household_id = ? WHERE id = ?`, id, userID); err != nil {
		return 0, fmt.Errorf("set active household: %w", err)
	}
	return id, nil
}

// CreateSharedHousehold makes a new shared budget with the caller as owner and
// switches them into it.
func (s *Store) CreateSharedHousehold(ctx context.Context, userID int64, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("give the household a name")
	}
	if len(name) > 60 {
		return 0, fmt.Errorf("that name is too long")
	}

	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// personal_for stays NULL: this is a shared household, and the partial
		// unique index only constrains personal ones.
		res, err := tx.ExecContext(ctx, `INSERT INTO households(name) VALUES(?)`, name)
		if err != nil {
			return fmt.Errorf("create household: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`,
			id, userID); err != nil {
			return fmt.Errorf("add owner: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET active_household_id = ? WHERE id = ?`, id, userID)
		return err
	})
	return id, err
}

// ── resolving the caller's household ──────────────────────────────────────────

// ActiveHousehold returns the household the user is working in and their role in it.
func (s *Store) ActiveHousehold(ctx context.Context, u User) (Membership, error) {
	m, err := s.activeHouseholdOnce(ctx, u.ID)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Membership{}, err
	}

	// Fall back to the personal household, creating it if it is missing.
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM households WHERE personal_for = ?`, u.ID)
		switch scanErr := row.Scan(&id); {
		case errors.Is(scanErr, sql.ErrNoRows):
			var mkErr error
			if id, mkErr = createPersonalHousehold(ctx, tx, u.ID, u.DisplayName); mkErr != nil {
				return mkErr
			}
		case scanErr != nil:
			return scanErr
		default:
			// It exists; make sure the membership row does too before pointing
			// the user at it, or the next request would fall through here again.
			if _, execErr := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO household_members(household_id, user_id, role)
				 VALUES(?, ?, 'owner')`, id, u.ID); execErr != nil {
				return execErr
			}
			if _, execErr := tx.ExecContext(ctx,
				`UPDATE users SET active_household_id = ? WHERE id = ?`, id, u.ID); execErr != nil {
				return execErr
			}
		}
		return nil
	})
	if err != nil {
		return Membership{}, fmt.Errorf("resolve household: %w", err)
	}
	return s.activeHouseholdOnce(ctx, u.ID)
}

// activeHouseholdOnce reads the active household, returning ErrNotFound if the pointer
// is null or no longer backed by a membership.
func (s *Store) activeHouseholdOnce(ctx context.Context, userID int64) (Membership, error) {
	var m Membership
	var personal sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT h.id, h.name, h.personal_for, hm.role,
		       (SELECT COUNT(*) FROM household_members x WHERE x.household_id = h.id)
		FROM users u
		JOIN households         h  ON h.id = u.active_household_id
		JOIN household_members  hm ON hm.household_id = h.id AND hm.user_id = u.id
		WHERE u.id = ?`, userID,
	).Scan(&m.ID, &m.Name, &personal, &m.Role, &m.Members)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, fmt.Errorf("select active household: %w", err)
	}
	m.Personal = personal.Valid
	return m, nil
}

// HouseholdsFor lists every household the user belongs to, for the switcher.
func (s *Store) HouseholdsFor(ctx context.Context, userID int64) ([]Household, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.name, h.personal_for,
		       (SELECT COUNT(*) FROM household_members x WHERE x.household_id = h.id)
		FROM household_members hm
		JOIN households h ON h.id = hm.household_id
		WHERE hm.user_id = ?
		ORDER BY (h.personal_for IS NULL), h.name COLLATE NOCASE, h.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list households: %w", err)
	}
	defer rows.Close()

	var out []Household
	for rows.Next() {
		var h Household
		var personal sql.NullInt64
		if err := rows.Scan(&h.ID, &h.Name, &personal, &h.Members); err != nil {
			return nil, err
		}
		h.Personal = personal.Valid
		out = append(out, h)
	}
	return out, rows.Err()
}

// SwitchHousehold points the user at a different household.
func (s *Store) SwitchHousehold(ctx context.Context, userID, householdID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET active_household_id = ?
		WHERE id = ?
		  AND EXISTS (SELECT 1 FROM household_members
		              WHERE household_id = ? AND user_id = ?)`,
		householdID, userID, householdID, userID)
	if err != nil {
		return fmt.Errorf("switch household: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotMember
	}
	return nil
}

// ── administering a household ─────────────────────────────────────────────────

// RenameHousehold changes the display name of a shared household.
func (s *Store) RenameHousehold(ctx context.Context, householdID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("give the household a name")
	}
	if len(name) > 60 {
		return fmt.Errorf("that name is too long")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE households SET name = ? WHERE id = ?`, name, householdID)
	if err != nil {
		return fmt.Errorf("rename household: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Members lists a household's people, owners first.
func (s *Store) Members(ctx context.Context, householdID, selfID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, ''),
		       hm.role, hm.joined_at
		FROM household_members hm
		JOIN users u ON u.id = hm.user_id
		WHERE hm.household_id = ?
		ORDER BY CASE hm.role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 ELSE 2 END,
		         u.display_name COLLATE NOCASE, u.id`, householdID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		m.IsSelf = m.UserID == selfID
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetRole changes one member's role. Demoting the last owner is refused, and the count
// and the update share a transaction, so two owners cannot simultaneously demote each
// other and leave the household with none.
func (s *Store) TransferOwnership(ctx context.Context, householdID, fromUserID, toUserID int64) error {
	if fromUserID == toUserID {
		return fmt.Errorf("you already own this budget")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// The caller must actually be an owner. Checked here rather than relying
		// on the route's permission wrapper alone, because this is the one action
		// that gives away the ability to perform it.
		var mine Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, fromUserID).Scan(&mine)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if mine != RoleOwner {
			return ErrForbidden
		}

		var theirs Role
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, toUserID).Scan(&theirs)
		if errors.Is(err, sql.ErrNoRows) {
			// Only an existing member can be promoted. Handing a budget to an address that has
			// not accepted an invitation would leave it owned by nobody who can sign in.
			return ErrNotMember
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = 'owner'
			 WHERE household_id = ? AND user_id = ?`, householdID, toUserID); err != nil {
			return fmt.Errorf("promote new owner: %w", err)
		}
		// Demoted to editor rather than removed: the previous owner almost
		// certainly still uses the budget, and quietly ejecting them would be a
		// surprising way for a transfer to end.
		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = 'editor'
			 WHERE household_id = ? AND user_id = ?`, householdID, fromUserID); err != nil {
			return fmt.Errorf("step down: %w", err)
		}

		// Belt and braces: prove the invariant this method exists to protect
		// before committing, rather than trusting the two statements above.
		owners, err := ownerCount(ctx, tx, householdID)
		if err != nil {
			return err
		}
		if owners != 1 {
			return fmt.Errorf("transfer would leave %d owners, refusing", owners)
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: fromUserID},
			"transferred ownership", "member", toUserID, "and stepped down to editor")
	})
}

func (s *Store) SetRole(ctx context.Context, householdID, actorID, targetID int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("unknown role")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var current Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if current == role {
			return nil
		}

		if current == RoleOwner {
			if err := requireAnotherOwner(ctx, tx, householdID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = ? WHERE household_id = ? AND user_id = ?`,
			role, householdID, targetID); err != nil {
			return err
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: actorID},
			"changed a role", "member", targetID, fmt.Sprintf("to %s", role))
	})
}

func ownerCount(ctx context.Context, tx *sql.Tx, householdID int64) (int, error) {
	var owners int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM household_members WHERE household_id = ? AND role = 'owner'`,
		householdID).Scan(&owners)
	return owners, err
}

// requireAnotherOwner refuses to demote or remove the only owner, which would
// leave a household nobody could administer. Called inside the same transaction
// as the change, so two owners cannot simultaneously demote each other.
func requireAnotherOwner(ctx context.Context, tx *sql.Tx, householdID int64) error {
	owners, err := ownerCount(ctx, tx, householdID)
	if err != nil {
		return err
	}
	if owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// RemoveMember takes somebody out of a household.
func (s *Store) RemoveMember(ctx context.Context, householdID, actorID, targetID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var role Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}

		if role == RoleOwner {
			if err := requireAnotherOwner(ctx, tx, householdID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID); err != nil {
			return err
		}

		// Anyone left pointing at this household is moved back to their own, or their pointer
		// is cleared for ActiveHousehold to repair.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET active_household_id = (SELECT id FROM households WHERE personal_for = users.id)
			WHERE id = ? AND active_household_id = ?`, targetID, householdID); err != nil {
			return err
		}
		// Their entries stay; only the membership goes.
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: actorID},
			"removed a member", "member", targetID, "their entries were kept")
	})
}

// DeleteHousehold removes a shared household and everything in it.
func (s *Store) DeleteHousehold(ctx context.Context, householdID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var personal sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT personal_for FROM households WHERE id = ?`, householdID).Scan(&personal)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if personal.Valid {
			return ErrPersonalHousehold
		}

		// Move everyone out first. users.active_household_id is ON DELETE SET
		// NULL, so this is belt and braces -- but it means members land back in
		// their own budget rather than on a page that has to repair itself.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET active_household_id = (SELECT id FROM households WHERE personal_for = users.id)
			WHERE active_household_id = ?`, householdID); err != nil {
			return err
		}

		// The household's transactions, funds, buckets, allocations and budgets
		// all cascade from this one DELETE.
		_, err = tx.ExecContext(ctx, `DELETE FROM households WHERE id = ?`, householdID)
		return err
	})
}

// LeaveHousehold is a member removing themselves.
func (s *Store) LeaveHousehold(ctx context.Context, householdID, userID int64) error {
	var personal sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT personal_for FROM households WHERE id = ?`, householdID).Scan(&personal)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if personal.Valid {
		return ErrPersonalHousehold
	}
	// Leaving is removing yourself, so the actor and the target are the same
	// person — which is exactly what the history should say.
	return s.RemoveMember(ctx, householdID, userID, userID)
}

// ── invitations ───────────────────────────────────────────────────────────────

// InviteMember records an invitation for an email address.
const InviteTTL = 24 * time.Hour

var inviteTTLModifier = after(InviteTTL)

// ErrInviteExpired is returned when an invitation is real but too old to use.
var ErrInviteExpired = errors.New("invitation expired")

func (s *Store) InviteMember(ctx context.Context, householdID, invitedBy int64, email string, role Role) error {
	email = NormalizeEmail(email)
	if email == "" {
		return fmt.Errorf("enter an email address")
	}
	if role != RoleEditor && role != RoleViewer {
		return fmt.Errorf("invite someone as an editor or a viewer")
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Already in? Say so plainly rather than creating an invitation that
		// could never be accepted.
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM household_members hm
			JOIN users u ON u.id = hm.user_id
			WHERE hm.household_id = ?
			  AND (u.email = ? COLLATE NOCASE OR u.username = ? COLLATE NOCASE)`,
			householdID, email, email).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyMember
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO household_invites(household_id, email, role, invited_by, expires_at)
			VALUES(?, ?, ?, ?, datetime('now', ?))`,
			householdID, email, role, invitedBy, inviteTTLModifier)
		if err != nil {
			// idx_invites_open is partial on status='pending', so this can only
			// mean an unanswered invitation already exists.
			if isUniqueViolation(err) {
				return ErrInviteOpen
			}
			return fmt.Errorf("create invite: %w", err)
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: invitedBy},
			"invited", "invitation", 0, fmt.Sprintf("%s as %s", email, role))
	})
}

// PendingInvites lists a household's unanswered invitations, for its settings page.
func (s *Store) PendingInvites(ctx context.Context, householdID int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.household_id, i.email, i.role, i.created_at,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, '')),
		       IFNULL(i.expires_at, ''),
		       i.expires_at IS NOT NULL AND i.expires_at <= datetime('now')
		FROM household_invites i
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.household_id = ? AND i.status = 'pending'
		ORDER BY i.created_at DESC, i.id DESC`, householdID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.HouseholdID, &i.Email, &i.Role,
			&i.CreatedAt, &i.InvitedBy, &i.ExpiresAt, &i.Expired); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ResendInvite gives an unanswered invitation another 24 hours.
func (s *Store) ResendInvite(ctx context.Context, householdID, inviteID int64) (Invite, error) {
	var inv Invite
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE household_invites
			SET expires_at = datetime('now', ?)
			WHERE id = ? AND household_id = ? AND status = 'pending'`,
			inviteTTLModifier, inviteID, householdID)
		if err != nil {
			return fmt.Errorf("extend invite: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return tx.QueryRowContext(ctx, `
			SELECT i.id, i.household_id, h.name, i.email, i.role, IFNULL(i.expires_at, '')
			FROM household_invites i
			JOIN households h ON h.id = i.household_id
			WHERE i.id = ?`, inviteID,
		).Scan(&inv.ID, &inv.HouseholdID, &inv.HouseholdName, &inv.Email,
			&inv.Role, &inv.ExpiresAt)
	})
	return inv, err
}

// TestOnlyExpireInvite ages an invitation past its window.
func (s *Store) TestOnlyExpireInvite(ctx context.Context, inviteID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE household_invites SET expires_at = datetime('now', '-1 minute') WHERE id = ?`,
		inviteID)
	return err
}

// PurgeStaleInvites removes invitations that expired long enough ago that nobody will
// act on them.
func (s *Store) PurgeStaleInvites(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM household_invites
		WHERE status = 'pending'
		  AND expires_at IS NOT NULL
		  AND expires_at <= datetime('now', '-30 days')`)
	if err != nil {
		return 0, fmt.Errorf("purge stale invites: %w", err)
	}
	return res.RowsAffected()
}

// InvitesFor lists unanswered invitations addressed to one person, matched case-
// insensitively.
func (s *Store) InvitesFor(ctx context.Context, userID int64, email string) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.household_id, h.name, i.role, i.created_at,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, '')),
		       IFNULL(i.expires_at, '')
		FROM household_invites i
		JOIN households h ON h.id = i.household_id
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.status = 'pending'
		  AND i.email = ? COLLATE NOCASE
		  -- A NULL expiry compares as NULL, which is not true, so it is treated
		  -- as expired. Failing closed is the right direction for something that
		  -- grants access to a budget: the worst case is an owner resending.
		  AND i.expires_at > datetime('now')
		  AND NOT EXISTS (SELECT 1 FROM household_members m
		                  WHERE m.household_id = i.household_id AND m.user_id = ?)
		ORDER BY i.created_at ASC, i.id ASC`, NormalizeEmail(email), userID)
	if err != nil {
		return nil, fmt.Errorf("list my invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.HouseholdID, &i.HouseholdName, &i.Role,
			&i.CreatedAt, &i.InvitedBy, &i.ExpiresAt); err != nil {
			return nil, err
		}
		i.Email = NormalizeEmail(email)
		out = append(out, i)
	}
	return out, rows.Err()
}

// AcceptInvite turns an invitation into a membership and switches the user into the
// household.
func (s *Store) AcceptInvite(ctx context.Context, inviteID, userID int64, email string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var householdID int64
		var role Role
		var expired bool
		err := tx.QueryRowContext(ctx, `
			SELECT household_id, role,
			       expires_at IS NULL OR expires_at <= datetime('now')
			FROM household_invites
			WHERE id = ? AND status = 'pending' AND email = ? COLLATE NOCASE`,
			inviteID, NormalizeEmail(email)).Scan(&householdID, &role, &expired)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Checked inside the transaction, not before it.
		if expired {
			return ErrInviteExpired
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO household_members(household_id, user_id, role)
			VALUES(?, ?, ?)`, householdID, userID, role); err != nil {
			return fmt.Errorf("join household: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE household_invites
			SET status = 'accepted', responded_at = datetime('now')
			WHERE id = ?`, inviteID); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET active_household_id = ? WHERE id = ?`, householdID, userID); err != nil {
			return err
		}
		// The actor is the person joining, which is why this is recorded here
		// rather than where the invitation was sent: accepting is their act.
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: userID},
			"joined", "member", userID, fmt.Sprintf("as %s", role))
	})
}

// DeclineInvite marks an invitation refused. The row is kept rather than deleted so the
// inviter can see what happened, and because idx_invites_open is partial on
// status='pending' a fresh invitation can still be sent later.
func (s *Store) DeclineInvite(ctx context.Context, inviteID int64, email string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE household_invites
		SET status = 'declined', responded_at = datetime('now')
		WHERE id = ? AND status = 'pending' AND email = ? COLLATE NOCASE`,
		inviteID, NormalizeEmail(email))
	if err != nil {
		return fmt.Errorf("decline invite: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeInvite withdraws an invitation the household sent.
func (s *Store) RevokeInvite(ctx context.Context, inviteID, householdID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE household_invites
		SET status = 'revoked', responded_at = datetime('now')
		WHERE id = ? AND household_id = ? AND status = 'pending'`,
		inviteID, householdID)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ═════════════════════════════════════════════════════════════════════════════
// jobs.go
// ═════════════════════════════════════════════════════════════════════════════

// JobStatus tracks a receipt through the queue.
type JobStatus string

const (
	// JobQueued is waiting to be picked up.
	JobQueued JobStatus = "queued"
	// JobProcessing has been claimed by a worker.
	JobProcessing JobStatus = "processing"
	// JobDone finished successfully.
	JobDone JobStatus = "done"
	// JobFailed gave up. The user must be told.
	JobFailed JobStatus = "failed"
)

// DraftItem is one product line OCR read off a receipt.
type DraftItem struct {
	Description string `json:"description"`
	Amount      Cents  `json:"amount"`
}

// ReceiptDraft is what OCR made of a receipt: a proposal, never a fact. Nothing
// in it has reached the ledger, and nothing will until a person has seen these
// numbers next to the photograph they came from and pressed Save.
//
// Everything is optional. A receipt photographed in bad light yields a draft
// with nothing but Text, which is still an improvement on nothing: the user gets
// the image on screen beside an empty form instead of having to find it again.
type ReceiptDraft struct {
	Merchant string `json:"merchant,omitempty"`
	Category string `json:"category,omitempty"`
	Date     string `json:"date,omitempty"`

	Total    Cents `json:"total,omitempty"`
	Subtotal Cents `json:"subtotal,omitempty"`
	Tax      Cents `json:"tax,omitempty"`
	Tip      Cents `json:"tip,omitempty"`

	Items []DraftItem `json:"items,omitempty"`

	// Confidence is 0..1. It decides how emphatically the form asks the user to
	// check the number, and nothing else.
	Confidence float64 `json:"confidence,omitempty"`

	// Text is the raw OCR output, shown on request and kept for diagnosis.
	Text string `json:"-"`
}

// HasTotal reports whether there is an amount worth prefilling.
func (d ReceiptDraft) HasTotal() bool { return d.Total > 0 }

// Certainty buckets the confidence for the interface, which needs three states
// rather than a number: a percentage implies a precision this does not have.
func (d ReceiptDraft) Certainty() string {
	switch {
	case !d.HasTotal():
		return "none"
	case d.Confidence >= 0.85:
		return "high"
	case d.Confidence >= 0.55:
		return "medium"
	default:
		return "low"
	}
}

// ItemsTotal is the sum of the draft's line items, for a template that wants to
// show whether they reconcile.
func (d ReceiptDraft) ItemsTotal() Cents {
	var sum Cents
	for _, it := range d.Items {
		sum += it.Amount
	}
	return sum
}

// ReceiptJob is one queued receipt image.
type ReceiptJob struct {
	ID int64

	// UserID is who uploaded the file; HouseholdID is which budget the expense will belong
	// to.
	UserID      int64
	HouseholdID int64

	Path          string
	OriginalName  string
	Status        JobStatus
	Error         string
	Attempts      int
	TransactionID *int64
	CreatedAt     string
	FinishedAt    string

	// Draft is what OCR read, or nil when the receipt has not been processed or
	// nothing could be read from it.
	Draft *ReceiptDraft
}

// receiptJobColumns is the column list every receipt_jobs read shares, so a
// column added to the table is added to one place rather than four.
const receiptJobColumns = `id, user_id, household_id, path, original_name, status,
	error, attempts, transaction_id, created_at, IFNULL(finished_at, ''),
	parsed_total_cents, parsed_confidence, parsed_json, ocr_text`

// scanReceiptJob reads one row in the order receiptJobColumns lists.
func scanReceiptJob(row rowScanner) (ReceiptJob, error) {
	var j ReceiptJob
	var total int64
	var confidence float64
	var parsed, text string

	if err := row.Scan(&j.ID, &j.UserID, &j.HouseholdID, &j.Path, &j.OriginalName,
		&j.Status, &j.Error, &j.Attempts, &j.TransactionID, &j.CreatedAt,
		&j.FinishedAt, &total, &confidence, &parsed, &text); err != nil {
		return ReceiptJob{}, err
	}

	// A draft exists only once the worker has written one. An empty column is
	// the normal state for a job that is still queued.
	if parsed == "" && total == 0 && text == "" {
		return j, nil
	}

	d := ReceiptDraft{}
	if parsed != "" {
		if err := json.Unmarshal([]byte(parsed), &d); err != nil {
			// A draft that will not decode is a bug in this package, not a
			// reason to fail the user's page: the receipt is still there and can
			// still be typed in by hand.
			log.Printf("store: receipt job %d has an undecodable draft: %v", j.ID, err)
			d = ReceiptDraft{}
		}
	}
	// The two promoted columns are authoritative over the document, because they
	// are what any query filters or sorts on.
	d.Total = Cents(total)
	d.Confidence = confidence
	d.Text = text
	j.Draft = &d
	return j, nil
}

// SaveReceiptDraft records what OCR made of a receipt. It deliberately does not
// touch the transactions table: the worker proposes, and only the user disposes.
func (s *Store) SaveReceiptDraft(ctx context.Context, jobID int64, d ReceiptDraft) error {
	// Text travels in its own column, so it is cleared before the document is
	// encoded rather than being stored twice.
	text := d.Text
	d.Text = ""

	blob, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode receipt draft: %w", err)
	}

	// The OCR text of a long receipt is a few kilobytes; a pathological image
	// could produce far more, and none of it past the first few thousand
	// characters is any use to anybody.
	const maxText = 20_000
	if len(text) > maxText {
		text = text[:maxText]
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET parsed_total_cents = ?, parsed_confidence = ?, parsed_json = ?, ocr_text = ?
		WHERE id = ?`,
		int64(d.Total), d.Confidence, string(blob), text, jobID)
	if err != nil {
		return fmt.Errorf("save receipt draft: %w", err)
	}
	return requireOneRow(res)
}

// MaxJobAttempts is how many times a receipt is retried before the user is told it
// failed.
const MaxJobAttempts = 3

// EnqueueReceipt adds a receipt to the processing queue and returns its id, so the
// handler returns immediately rather than holding the user on a spinner.
func (s *Store) EnqueueReceipt(ctx context.Context, sc Scope, path, originalName string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO receipt_jobs(user_id, household_id, path, original_name) VALUES(?, ?, ?, ?)`,
		sc.UserID, sc.HouseholdID, path, originalName)
	if err != nil {
		return 0, fmt.Errorf("enqueue receipt: %w", err)
	}
	return res.LastInsertId()
}

// ClaimReceiptJob atomically takes the oldest queued job.
func (s *Store) ClaimReceiptJob(ctx context.Context) (ReceiptJob, error) {
	var job ReceiptJob

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM receipt_jobs
			WHERE status = 'queued'
			ORDER BY id ASC
			LIMIT 1`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find queued job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE receipt_jobs
			SET status = 'processing', attempts = attempts + 1, started_at = ?
			WHERE id = ? AND status = 'queued'`,
			time.Now().UTC().Format(time.RFC3339), id)
		if err != nil {
			return fmt.Errorf("claim job: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Another worker got there first.
			return ErrNotFound
		}

		var scanErr error
		job, scanErr = scanReceiptJob(tx.QueryRowContext(ctx,
			`SELECT `+receiptJobColumns+` FROM receipt_jobs WHERE id = ?`, id))
		return scanErr
	})

	return job, err
}

// CompleteReceiptJob marks a job done, optionally linking the transaction it produced.
func (s *Store) CompleteReceiptJob(ctx context.Context, jobID int64, txID *int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET status = 'done', error = '', transaction_id = ?, finished_at = ?
		WHERE id = ?`, txID, time.Now().UTC().Format(time.RFC3339), jobID)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// RetryOrFailReceiptJob puts a job back in the queue, or gives up on it once
// MaxJobAttempts is reached.
func (s *Store) RetryOrFailReceiptJob(ctx context.Context, jobID int64, cause string) (givenUp bool, err error) {
	var attempts int
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts FROM receipt_jobs WHERE id = ?`, jobID).Scan(&attempts); err != nil {
		return false, fmt.Errorf("read attempts: %w", err)
	}

	if attempts >= MaxJobAttempts {
		_, err := s.db.ExecContext(ctx, `
			UPDATE receipt_jobs SET status = 'failed', error = ?, finished_at = ?
			WHERE id = ?`, cause, time.Now().UTC().Format(time.RFC3339), jobID)
		if err != nil {
			return false, fmt.Errorf("fail job: %w", err)
		}
		return true, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE receipt_jobs SET status = 'queued', error = ? WHERE id = ?`,
		cause, jobID); err != nil {
		return false, fmt.Errorf("requeue job: %w", err)
	}
	return false, nil
}

// RecoverStuckJobs returns jobs left in processing back to the queue.
func (s *Store) RecoverStuckJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE receipt_jobs SET status = 'queued' WHERE status = 'processing'`)
	if err != nil {
		return 0, fmt.Errorf("recover stuck jobs: %w", err)
	}
	return res.RowsAffected()
}

// UnattachedReceipt fetches a processed receipt that has not yet become an expense.
func (s *Store) UnattachedReceipt(ctx context.Context, sc Scope, jobID int64) (ReceiptJob, error) {
	j, err := scanReceiptJob(s.db.QueryRowContext(ctx, `
		SELECT `+receiptJobColumns+`
		FROM receipt_jobs
		WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
		jobID, sc.HouseholdID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptJob{}, ErrNotFound
	}
	if err != nil {
		return ReceiptJob{}, fmt.Errorf("unattached receipt: %w", err)
	}
	return j, nil
}

// UnattachedReceipts lists processed receipts in this budget that nobody has turned into
// an expense. Without it an upload is a dead end whenever the notification is missed.
func (s *Store) UnattachedReceipts(ctx context.Context, sc Scope, limit int) ([]ReceiptJob, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+receiptJobColumns+`
		FROM receipt_jobs
		WHERE household_id = ? AND transaction_id IS NULL AND status = 'done'
		ORDER BY id DESC
		LIMIT ?`, sc.HouseholdID, limit)
	if err != nil {
		return nil, fmt.Errorf("unattached receipts: %w", err)
	}
	defer rows.Close()

	out := []ReceiptJob{}
	for rows.Next() {
		j, err := scanReceiptJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unattached receipt: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DiscardReceipt throws away an uploaded receipt nobody wants, returning the stored
// path so the caller can delete the file too.
func (s *Store) DiscardReceipt(ctx context.Context, sc Scope, jobID int64) (string, error) {
	var path string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT path FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
			jobID, sc.HouseholdID).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			// Either it belongs to another budget, or it is already an expense.
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read receipt job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			DELETE FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
			jobID, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("discard receipt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// AttachReceipt links a processed receipt to the expense it became.
func (s *Store) AttachReceipt(ctx context.Context, sc Scope, jobID, txID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var path, name string
		err := tx.QueryRowContext(ctx, `
			SELECT path, original_name FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
			jobID, sc.HouseholdID).Scan(&path, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read receipt job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE transactions SET receipt_path = ?, receipt_name = ?
			WHERE id = ? AND household_id = ?`, path, name, txID, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("attach receipt to transaction: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE receipt_jobs SET transaction_id = ?
			WHERE id = ? AND transaction_id IS NULL`, txID, jobID); err != nil {
			return fmt.Errorf("mark receipt attached: %w", err)
		}
		return nil
	})
}

// PendingReceiptCount is how many receipts destined for this budget are still in
// flight.
func (s *Store) PendingReceiptCount(ctx context.Context, sc Scope) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM receipt_jobs
		 WHERE household_id = ? AND status IN ('queued','processing')`,
		sc.HouseholdID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pending receipts: %w", err)
	}
	return n, nil
}

// ═════════════════════════════════════════════════════════════════════════════
// notifications.go
// ═════════════════════════════════════════════════════════════════════════════

// Notification is a message waiting for a user, stored in a table rather than the
// session: an unseen row keeps waiting however long the user stays away, and survives
// a server restart.
type Notification struct {
	ID        int64
	Kind      string // "info" | "success" | "error"
	Text      string
	Link      string
	CreatedAt string
}

// Notify records a message for a user.
func (s *Store) Notify(ctx context.Context, userID int64, kind, text, link string) error {
	switch kind {
	case "info", "success", "error":
	default:
		kind = "info"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications(user_id, kind, text, link) VALUES(?, ?, ?, ?)`,
		userID, kind, text, link)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// TakeNotifications returns a user's unseen messages and marks them seen.
func (s *Store) TakeNotifications(ctx context.Context, userID int64) ([]Notification, error) {
	out := []Notification{}

	// Reading and marking seen are one transaction, because "take" is the whole
	// contract: two pages loading together -- a tab and its own refresh -- would
	// otherwise both read the same unseen rows before either marked them, and
	// the user would get the same "your receipt is ready" toast twice.
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, kind, text, link, created_at
			FROM notifications
			WHERE user_id = ? AND seen_at IS NULL
			ORDER BY id ASC
			LIMIT 20`, userID)
		if err != nil {
			return fmt.Errorf("read notifications: %w", err)
		}
		var ids []any
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.Kind, &n.Text, &n.Link, &n.CreatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan notification: %w", err)
			}
			out = append(out, n)
			ids = append(ids, n.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}

		ph := make([]byte, 0, len(ids)*2)
		for i := range ids {
			if i > 0 {
				ph = append(ph, ',')
			}
			ph = append(ph, '?')
		}
		// user_id in the WHERE as well as the ids: the ids came from a scoped
		// read, so this changes nothing today, but it means a future caller
		// cannot turn this into a way to mark somebody else's rows seen.
		args := append([]any{time.Now().UTC().Format(time.RFC3339)}, ids...)
		args = append(args, userID)

		if _, err := tx.ExecContext(ctx,
			`UPDATE notifications SET seen_at = ? WHERE id IN (`+string(ph)+`) AND user_id = ?`,
			args...); err != nil {
			return fmt.Errorf("mark notifications seen: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
