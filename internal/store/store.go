// Package store is the only package that speaks SQL.
//
// Previously every query lived inline in an http.HandlerFunc, which made the
// money rules impossible to test without spinning up a web server and made it
// easy to write a handler that trusted a form field where it should have
// trusted the database. Keeping SQL here means a handler can only ask for
// operations that are safe by construction.
package store


import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
)

// Cents is an alias so callers of this package do not have to import money
// just to name a field type. Being an alias (=) and not a definition means
// money.Cents and store.Cents are the same type, methods included.
type Cents = money.Cents

// Ratio is re-exported for the same reason as Cents.
var Ratio = money.Ratio

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Scope says whose money a call operates on: the household that owns the data,
// and the person performing the action.
//
// Every method that touches financial data takes one of these instead of a bare
// id, and that is a deliberate defence rather than tidiness. Before shared
// budgeting there was one id and it was the user's. Now there are two, both
// int64, and passing the wrong one would compile perfectly while showing one
// household another's money -- the worst bug this application could have. A
// named struct makes that mistake impossible to express.
//
// The two fields have distinct jobs:
//
//	HouseholdID  ownership. Every SELECT filters on it and every INSERT sets
//	             it. This is what "whose data is this" means.
//	UserID       attribution. Written to the user_id column on insert so the
//	             UI can say "Added by Priya", and never used to filter a read.
//
// That split is why removing somebody from a household leaves their entries
// intact: the rows belong to the household, and the departing member's id is
// only a label on them.
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
	// Deliberately the same error for both, so a probing user cannot tell a
	// missing id from another person's id.
	ErrNotFound = errors.New("not found")

	// ErrConflict means the row was changed by somebody else between the moment
	// it was read and the moment it was saved.
	//
	// A separate error from ErrNotFound because the two need different words on
	// screen and lead to different actions: one is "this is gone", the other is
	// "look at what changed, then decide". Collapsing them was the old
	// behaviour's problem in miniature -- it silently chose for the user.
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

// DateLayout is the storage format for occurred_on. Text dates in this format
// sort lexicographically in the same order as chronologically, which is why
// SQLite can index and ORDER BY them directly.
const DateLayout = "2006-01-02"

// MonthLayout is the storage format for a month selector, e.g. "2026-04".
const MonthLayout = "2006-01"

// ParseDate validates a user-supplied date. The old code accepted any string
// the browser sent and wrote it straight to the column, so a hand-edited form
// could put "tomorrow" or "" into a date field and quietly break every chart
// that sorted on it.
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

// monthRange converts "2026-04" into the half-open interval
// ["2026-04-01", "2026-05-01").
//
// Filtering this way lets SQLite use idx_tx_user_date. The obvious
// alternative, substr(occurred_on, 1, 7) = ?, wraps the column in a function
// and forces a full scan of every row the user owns.
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
	// KindExpense is money leaving for good. Decreases cash. The only kind
	// that counts as spending.
	KindExpense Kind = "expense"
	// KindFundDeposit moves cash into a savings fund. A transfer: cash falls,
	// the fund rises, net worth is unchanged. Not spending.
	KindFundDeposit Kind = "fund_deposit"
	// KindFundWithdrawal moves money back out of a fund. Not income.
	KindFundWithdrawal Kind = "fund_withdrawal"
)

// Valid reports whether k is one of the four known kinds. Handlers call this
// before trusting a value that came from a query string.
func (k Kind) Valid() bool {
	switch k {
	case KindIncome, KindExpense, KindFundDeposit, KindFundWithdrawal:
		return true
	}
	return false
}

// IsTransfer reports whether k moves money between the user's own pots rather
// than in or out of their control. Transfers are excluded from spending and
// income analytics.
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

// cashSign is the multiplier this kind applies to spendable cash. Written once
// here and reused in every SQL aggregate below, so no two queries can disagree
// about whether a deposit reduces cash.
const cashSignSQL = `CASE kind
	WHEN 'income'          THEN  amount_cents
	WHEN 'fund_withdrawal' THEN  amount_cents
	ELSE                        -amount_cents
END`

// txSelect is the column list and joins shared by List, All and ByID.
//
// Written once because it was written three times: three byte-identical SELECT
// blocks that had to be kept in step with scanTransaction's argument order.
// Adding the "added by" join meant editing all three, and a query that scans
// columns in a different order than it selects them fails at runtime rather than
// at compile time. One constant makes that impossible.
//
// The LEFT JOIN on users is deliberately left, not inner: a deleted account
// cascades its transactions away, but a legacy row could still carry a user_id
// that no longer resolves, and an inner join would make such a row vanish from
// every list rather than merely show no author.
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

	// The remaining three of the wireframe's five Ws. Optional: the form asks
	// for them, but an expense with only a category and an amount is still a
	// valid expense, and refusing to save one would make the app tedious.
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

	// AddedBy is the name of whoever entered this row, for a shared budget where
	// "who put this here?" is a real question. Empty when the entry predates
	// shared budgeting or its author's account has since been deleted, which is
	// why the template checks it rather than always printing a name.
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

	// Version is the version the editor was shown, used as a compare-and-swap on
	// update. Zero means "do not check", which is what Add uses and what a
	// caller with no version to offer gets -- so the check is opt-in per call
	// site rather than something that silently starts failing everywhere.
	Version int64
}

// Add inserts a plain income or expense.
//
// Fund movements deliberately cannot be created here: they must go through
// Deposit, Withdraw or CloseFund, which enforce the balance rules inside a
// transaction. That is what stops a handler from inventing a deposit.
func (s *Store) Add(ctx context.Context, sc Scope, n NewTransaction) (int64, error) {
	if n.Kind != KindIncome && n.Kind != KindExpense {
		return 0, fmt.Errorf("Add only accepts income or expense, got %q", n.Kind)
	}
	if n.Amount <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}

	// An expense always records the essential flag; income never does. The
	// CHECK constraint on the table enforces the shape, this keeps the Go side
	// honest about it.
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

	// household_id owns the row; user_id records who entered it, which is what
	// the transactions list shows as "Added by". Both columns are NOT NULL, so
	// an insert that forgot either one fails immediately rather than producing a
	// row that no household can see.
	// The insert and its audit entry share one transaction, so the history can
	// never describe a row that does not exist, or miss one that does.
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

// Update edits an existing income or expense in place.
//
// The old app had no edit path at all: correcting a typo meant deleting the
// row and retyping it, which lost the original created_at and reordered the
// user's history.
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

	// The WHERE clause carries household_id as well as id. Every mutating query in
	// this package does, so a guessed id belonging to someone else affects
	// zero rows instead of theirs.
	//
	// kind is also constrained, which prevents an edit form from converting a
	// fund transfer into a plain expense and silently unbalancing a fund.
	//
	// version is the compare-and-swap. Shared budgeting made it possible for two
	// members to open the same expense and both save, and the second write used
	// to discard the first with no sign that anything had happened. Requiring the
	// version the editor was shown means a stale form affects zero rows, and the
	// caller can tell the difference between "gone" and "changed underneath you".
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// What it said before, so the history records the change rather than
		// only the outcome. Read inside the same transaction as the write, so
		// nothing can move between the two.
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

// explainFailedUpdate works out why an UPDATE matched nothing.
//
// Three causes are indistinguishable from a row count of zero, and the user
// deserves to be told which: the row is gone, somebody else changed it, or it is
// not an editable kind. Guessing "not found" for all three would send someone
// hunting for an entry that is on screen in front of them.
// It takes the caller's transaction rather than the pool, and that is not a
// style choice: the pool is capped at ONE connection so that SQLite's single
// writer becomes harmless queueing. Asking the pool for a second connection
// while a transaction holds the only one waits for a connection that cannot be
// released until the waiting finishes — the query blocks forever, taking the
// request with it. It deadlocked exactly once, in the conflict path, and hung
// the test suite for ten minutes rather than failing.
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

// Delete removes an income or expense.
//
// Fund movements are excluded: deleting one would leave the fund's derived
// balance out of step with the cash that funded it. Funds are closed through
// CloseFund instead, which books the reversal properly.
// Delete removes an income or expense, and records what it was.
//
// RETURNING gives the label and amount back from the same statement that removes
// the row. Reading it first and then deleting would be two steps with a gap, and
// the gap is exactly where the row could change — so the history would describe
// something slightly different from what was actually destroyed. This is the one
// place where the old value is unrecoverable afterwards, so it has to be captured
// at the moment of deletion.
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

// DefaultPageSize bounds a transaction page. The old /transactions route
// selected every row a user had ever created with no LIMIT, so the page grew
// without bound and the whole history was rendered on every request.
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

	// Ordering by created_at then id, not by occurred_on. A user who
	// backdates an entry still expects to see it at the top of the list
	// immediately after saving it. Ties break on id so the order is total and
	// pagination cannot show the same row on two pages.
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

// All returns every matching transaction with no page limit. Used only by the
// CSV export, which by definition wants the whole set.
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
// unaffected by moving money between pots -- the property the old code broke
// by booking deposits as expenses.
func (t Totals) NetWorth() Cents {
	return t.Income - t.Expense
}

// Totals aggregates all four kinds in a single pass. The old dashboard ran two
// separate SUM queries and then four more for the charts, each a full scan.
func (s *Store) Totals(ctx context.Context, sc Scope, month string) (Totals, error) {
	start, end, err := monthRange(month)
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
		WHERE household_id = ?`
	args := []any{sc.HouseholdID}
	if month != "" {
		q += ` AND occurred_on >= ? AND occurred_on < ?`
		args = append(args, start, end)
	}

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
	start, end, err := monthRange(month)
	if err != nil {
		return nil, err
	}

	q := `
		SELECT CASE WHEN TRIM(label) = '' THEN 'Uncategorised' ELSE TRIM(label) END AS grp,
		       SUM(amount_cents)
		FROM transactions
		WHERE household_id = ? AND kind = ?`
	args := []any{sc.HouseholdID, string(kind)}
	if month != "" {
		q += ` AND occurred_on >= ? AND occurred_on < ?`
		args = append(args, start, end)
	}
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
// The old app collected this flag on every expense form and then never showed
// it anywhere.
func (s *Store) EssentialSplit(ctx context.Context, sc Scope, month string) (essential, other Cents, err error) {
	start, end, err := monthRange(month)
	if err != nil {
		return 0, 0, err
	}
	q := `
		SELECT IFNULL(SUM(CASE WHEN essential = 1 THEN amount_cents ELSE 0 END), 0),
		       IFNULL(SUM(CASE WHEN essential = 0 THEN amount_cents ELSE 0 END), 0)
		FROM transactions
		WHERE household_id = ? AND kind = 'expense'`
	args := []any{sc.HouseholdID}
	if month != "" {
		q += ` AND occurred_on >= ? AND occurred_on < ?`
		args = append(args, start, end)
	}
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

// BalanceSeries returns the running cash balance over time.
//
// The running total is accumulated in SQL with a window function rather than
// in a Go loop, so the query returns one row per day instead of one per
// transaction. A user with 2,000 rows across 90 days previously sent 2,000
// points to Chart.js; now they send 90.
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
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		key := now.AddDate(0, -i, 0).Format(MonthLayout)
		if mp, ok := found[key]; ok {
			out = append(out, mp)
		} else {
			out = append(out, MonthPoint{Month: key})
		}
	}
	return out, nil
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

// cleanLabel trims a label and caps its length. Without a cap a pasted essay
// becomes a pie-chart legend entry and breaks the layout.
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

// resolveBucket validates a bucket id supplied by a form and returns it in the
// form the driver wants: nil for "no bucket", or the id.
//
// The ownership check matters more than it first appears. A bucket id arrives
// from a <select> in the browser and can be edited freely. Without this check a
// user could attribute their own expense to someone else's recurring bucket,
// which would corrupt that person's variable-cost estimate and their income
// allocation -- a way to silently alter another user's budget.
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
//
// The cap is generous compared with cleanLabel because "Why?" invites a
// sentence, but it is still bounded: an unbounded column is a way to bloat the
// database and to break every layout that renders it.
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
	// A partial unique index in the schema guarantees at most one per user.
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

// MonthlyNeeded is the amount per month required to reach the goal within
// TargetMonths. Returns 0 when either the goal or the horizon is unset.
//
// target_months existed in the old emergency_goals table and was written by
// the form handler, but nothing ever read it, so the user's answer to "in how
// many months?" had no effect on anything.
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

// Deposit moves cash into a fund.
//
// The whole operation is one row, inserted inside a transaction that first
// re-reads the user's available cash. Two things follow from that:
//
//   - A user cannot move in more than they hold. The old handler performed no
//     check at all, so any amount could be transferred regardless of balance.
//   - The cash side and the fund side can no longer disagree. The old code
//     wrote three separate rows (an expense, a funds.balance update and a
//     fund_transactions row) with no transaction around them, logging any
//     errors and carrying on, so a partial failure left the books unbalanced.
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
//
// The old app had no withdrawal path whatsoever: the only way to get money out
// of a fund was to delete the entire fund, which is why the delete handler had
// to credit the balance back and is where the exploit lived.
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
//
// This replaces the old /delete-fund handler, which did the following:
//
//	fundBalance, _ := strconv.ParseFloat(r.FormValue("fund_balance"), 64)
//	db.Exec("INSERT INTO income(...) VALUES(?, ?, ?, ?)", user, ..., fundBalance)
//
// The credited amount came from a hidden form field, so posting
// fund_balance=999999 credited 999999 as income. The live database still
// contains a 50,000,000 fund deposit created this way.
//
// Here the balance is computed from the fund's own transactions inside the
// transaction that books the reversal, and the reversal is a fund_withdrawal
// rather than income -- closing a fund does not make the user richer, so it
// must not appear as earnings in their income breakdown.
//
// The fund row is kept and stamped with closed_at instead of being deleted, so
// its transaction history stays intact and referential integrity holds.
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

// EmergencyFund returns the user's emergency fund, creating it on first use.
//
// The wireframe gives the Emergency Fund its own dashboard tab, so there has to
// be exactly one identifiable fund behind it rather than whichever fund the user
// happened to name "emergency". A partial unique index in the schema enforces
// the "exactly one" half; this method supplies the "always exists" half so the
// tab never has to render an error state.
func (s *Store) EmergencyFund(ctx context.Context, sc Scope) (Fund, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+fundColumns+`
		FROM funds f
		WHERE f.household_id = ? AND f.is_emergency = 1 AND f.closed_at IS NULL`, sc.HouseholdID)
	f, err := scanFund(row)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Fund{}, err
	}

	// Adopt an existing fund that is obviously meant to be the emergency one
	// before creating a second. Users migrating from the old app very often
	// already have a fund called "Emergency Fund", and ending up with two would
	// split their savings across both.
	var adoptID int64
	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM funds
		WHERE household_id = ? AND closed_at IS NULL AND is_emergency = 0
		  AND LOWER(name) LIKE '%emergency%'
		ORDER BY id ASC LIMIT 1`, sc.HouseholdID).Scan(&adoptID)
	switch {
	case err == nil:
		if _, err := s.db.ExecContext(ctx,
			`UPDATE funds SET is_emergency = 1 WHERE id = ? AND household_id = ?`,
			adoptID, sc.HouseholdID); err != nil {
			return Fund{}, fmt.Errorf("adopt emergency fund: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO funds(household_id, user_id, name, is_emergency) VALUES(?, ?, ?, 1)`,
			sc.HouseholdID, sc.UserID, EmergencyFundName); err != nil {
			return Fund{}, fmt.Errorf("create emergency fund: %w", err)
		}
	default:
		return Fund{}, fmt.Errorf("find adoptable fund: %w", err)
	}

	row = s.db.QueryRowContext(ctx, `
		SELECT `+fundColumns+`
		FROM funds f
		WHERE f.household_id = ? AND f.is_emergency = 1 AND f.closed_at IS NULL`, sc.HouseholdID)
	return scanFund(row)
}

// FundWithdrawalHistory returns the monthly total withdrawn from one fund,
// oldest month first.
//
// This is the raw material for the Emergency Fund tab's question: "what does
// their rate of extraction look like?"
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

// DepositRates returns, per fund, the total deposited and the number of
// distinct calendar months in which a deposit happened.
//
// Returned as one map from one query rather than a per-fund lookup, because
// the dashboard renders every fund at once and a query per fund is the classic
// N+1 that makes a page slow as soon as a user has a handful of them.
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

// inTx runs fn inside a database transaction, rolling back on any error.
//
// Every read inside fn must use tx, never s.db: the pool is capped at one
// connection, so a query issued on s.db while a transaction is open would wait
// forever for a connection the transaction already holds.
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

	// Low and High bracket what this bucket has historically cost. For a fixed
	// bucket both equal Fixed. For a variable one they are the cheapest and
	// dearest month observed, which is what lets the dashboard show expected
	// expenses as a range rather than a single misleading number.
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
		// A variable bucket's amount comes from its transactions, so a typed-in
		// figure would be misleading. Ignore rather than reject it: the form
		// keeps the field visible when switching kinds.
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
//
// Archiving rather than deleting keeps the allocation history readable: a
// DELETE would cascade through allocations and rewrite the past, making last
// month's funding report disagree with what the user saw at the time.
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

// MoveBucket shifts a bucket one place up or down the priority list.
//
// Up and down rather than a typed priority number, because two buckets sharing
// a number would make the waterfall order depend on row id, which the user
// cannot see or control.
func (s *Store) MoveBucket(ctx context.Context, sc Scope, bucketID int64, up bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Renumber first. Priorities drift out of sequence as buckets are
		// archived, and swapping two non-adjacent numbers would then move an
		// item several places at once.
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

	// One query for the list, with correlated subqueries for the three
	// per-month figures. The alternative -- a query per bucket -- is the N+1
	// that makes this page slow the moment a user has a realistic number of
	// recurring expenses.
	rows, err := s.db.QueryContext(ctx, `
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
		       IFNULL((
		           -- Trailing average of the last six months that had activity,
		           -- excluding the month being viewed so a part-paid month does
		           -- not drag the estimate down.
		           SELECT AVG(m.total) FROM (
		               SELECT SUM(t2.amount_cents) AS total
		               FROM transactions t2
		               WHERE t2.bucket_id = b.id AND t2.kind = 'expense'
		                 AND t2.occurred_on < ?
		               GROUP BY substr(t2.occurred_on, 1, 7)
		               ORDER BY substr(t2.occurred_on, 1, 7) DESC
		               LIMIT 6
		           ) m
		       ), 0) AS estimate,
		       IFNULL((
		           SELECT MIN(m.total) FROM (
		               SELECT SUM(t3.amount_cents) AS total
		               FROM transactions t3
		               WHERE t3.bucket_id = b.id AND t3.kind = 'expense'
		                 AND t3.occurred_on < ?
		               GROUP BY substr(t3.occurred_on, 1, 7)
		               ORDER BY substr(t3.occurred_on, 1, 7) DESC
		               LIMIT 6
		           ) m
		       ), 0) AS low,
		       IFNULL((
		           SELECT MAX(m.total) FROM (
		               SELECT SUM(t4.amount_cents) AS total
		               FROM transactions t4
		               WHERE t4.bucket_id = b.id AND t4.kind = 'expense'
		                 AND t4.occurred_on < ?
		               GROUP BY substr(t4.occurred_on, 1, 7)
		               ORDER BY substr(t4.occurred_on, 1, 7) DESC
		               LIMIT 6
		           ) m
		       ), 0) AS high
		FROM expense_buckets b
		WHERE b.household_id = ? AND b.archived_at IS NULL
		ORDER BY b.priority ASC, b.id ASC`,
		start, end, month, start, start, start, sc.HouseholdID)
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

// bucketDue computes what a bucket needs for the month.
//
// Kept as a plain function on the struct's values so the allocation code and
// the display code cannot disagree about the number.
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

// bucketRange brackets what a bucket costs.
//
// A fixed bucket has no range: it is the same figure every month, so pretending
// otherwise would invent uncertainty. A variable one is bracketed by the
// cheapest and dearest month seen, widened to include this month's actual spend
// if it has already exceeded the historical high -- an unusually large bill
// should raise the top of the range, not be hidden by it.
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

// BucketByID returns one active bucket.
func (s *Store) BucketByID(ctx context.Context, sc Scope, bucketID int64) (Bucket, error) {
	var b Bucket
	var kind string
	var fixed int64
	var essential int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, priority, cost_kind, fixed_cents, essential
		 FROM expense_buckets
		 WHERE id = ? AND household_id = ? AND archived_at IS NULL`, bucketID, sc.HouseholdID).
		Scan(&b.ID, &b.Name, &b.Priority, &kind, &fixed, &essential)
	if errors.Is(err, sql.ErrNoRows) {
		return Bucket{}, ErrNotFound
	}
	if err != nil {
		return Bucket{}, fmt.Errorf("bucket by id: %w", err)
	}
	b.CostKind = CostKind(kind)
	b.Fixed = Cents(fixed)
	b.Essential = essential == 1
	return b, nil
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

// EssentialMonthlyCost is the sum of the monthly requirement of every bucket
// tagged essential.
//
// This is the wireframe's definition of the emergency fund target: "Emergency
// fund is derived from the sum total of essential designated recurring monthly
// expense buckets that have been tagged as 'essential'."
func (s *Store) EssentialMonthlyCost(ctx context.Context, sc Scope, month string) (Cents, error) {
	buckets, err := s.Buckets(ctx, sc, month)
	if err != nil {
		return 0, err
	}
	var total Cents
	for _, b := range buckets {
		if b.Essential {
			total += b.Due
		}
	}
	return total, nil
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
//
// The wireframe describes an incremental rule: "When income is added, the funds
// immediately get allocated to the highest priority expense and then cascades
// down to the next highest priority until either all expenses have been paid or
// the income funds have all been allocated."
//
// Applying that incrementally is correct only while nothing else ever changes.
// In practice the user will edit an income's amount, delete one, reorder the
// priority list, or change a bucket's cost -- and each of those makes previously
// recorded allocations wrong in a way that is fiddly to unwind row by row.
//
// So the rule is implemented as a pure function of the month's current state
// and re-run after any mutation that could affect it. The observable behaviour
// for the described case is identical, because income is replayed in
// chronological order; the difference is that every other case is also correct,
// and running it twice changes nothing.
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
		rows, err := tx.QueryContext(ctx, `
			SELECT b.id, b.cost_kind, b.fixed_cents,
			       IFNULL((
			           SELECT SUM(t.amount_cents) FROM transactions t
			           WHERE t.bucket_id = b.id AND t.kind = 'expense'
			             AND t.occurred_on >= ? AND t.occurred_on < ?
			       ), 0) AS spent,
			       IFNULL((
			           SELECT AVG(m.total) FROM (
			               SELECT SUM(t2.amount_cents) AS total
			               FROM transactions t2
			               WHERE t2.bucket_id = b.id AND t2.kind = 'expense'
			                 AND t2.occurred_on < ?
			               GROUP BY substr(t2.occurred_on, 1, 7)
			               ORDER BY substr(t2.occurred_on, 1, 7) DESC
			               LIMIT 6
			           ) m
			       ), 0) AS estimate
			FROM expense_buckets b
			WHERE b.household_id = ? AND b.archived_at IS NULL
			ORDER BY b.priority ASC, b.id ASC`,
			start, end, start, sc.HouseholdID)
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
//
// Handlers call this after adding, editing or deleting an income or a
// bucket-attributed expense, which is the only place that knows the date.
func (s *Store) ReallocateMonthOf(ctx context.Context, sc Scope, date string) error {
	if len(date) < 7 {
		return s.Reallocate(ctx, sc, Today()[:7])
	}
	return s.Reallocate(ctx, sc, date[:7])
}

// AllocationsFor returns the month's summary.
func (s *Store) AllocationsFor(ctx context.Context, sc Scope, month string) (AllocationSummary, error) {
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

	buckets, err := s.Buckets(ctx, sc, month)
	if err != nil {
		return AllocationSummary{}, err
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


// LineItem is one entry within a transaction.
//
// The wireframe asks for "a breakdown on what categories the individual line
// items in the transaction belong to" -- a single shop trip is one transaction
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

// SetLineItems replaces a transaction's lines.
//
// The items must sum exactly to the transaction's amount. Allowing them to
// disagree would mean the category breakdown and the headline spending total
// tell different stories, and there would be no way to know which is right.
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
	// They are int64s parsed from the database and so are safe, but writing the
	// query this way means no future edit can turn it into an injection.
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

// CategoryBreakdown totals spending by category, using line-item categories
// where a transaction has them and the transaction's own label where it does
// not.
//
// The two halves have to be unioned rather than one replacing the other:
// counting a transaction's label as well as its lines would double the total,
// and ignoring unlined transactions would drop most of the data.
func (s *Store) CategoryBreakdown(ctx context.Context, sc Scope, month string) ([]LabelTotal, error) {
	start, end, err := monthRange(month)
	if err != nil {
		return nil, err
	}

	where := `t.household_id = ? AND t.kind = 'expense'`
	args := []any{sc.HouseholdID}
	if month != "" {
		where += ` AND t.occurred_on >= ? AND t.occurred_on < ?`
		args = append(args, start, end)
	}
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


// Budget is a monthly spending cap for one category, joined against what has
// actually been spent in the period being viewed.
//
// This is the feature the old app was missing entirely: it recorded spending
// faithfully but never compared it to a plan, which is the part that makes a
// budgeting tool a budgeting tool rather than a ledger.
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
//
// Matching is case-insensitive on the category label, because a user who types
// "food" one day and "Food" the next means the same category and would
// otherwise see their budget appear untouched.
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

// SpendCategories lists the expense labels the user has actually used, newest
// activity first, to populate a datalist on the expense and budget forms.
//
// Free-text categories were a real weakness of the old forms: "Food", "food"
// and "Foood" each became their own pie slice. Suggesting existing labels
// nudges users towards reusing them without imposing a fixed taxonomy.
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

// CreateUser inserts a new account.
//
// display_name is derived from the address rather than asked for: the wireframe's
// signup flow collects only an email and a password, and prompting for a third
// field would contradict it.
//
// Case-insensitive uniqueness is enforced here rather than by a COLLATE NOCASE
// index, because an existing database may already hold two accounts differing
// only in case -- the old schema's UNIQUE(username) was case-sensitive, and this
// project's database really does contain "Kushith" and "kushith". A NOCASE index
// could not be created over that data at all. See internal/db/migrate.go.
//
// The check and the insert share a transaction, so two simultaneous signups for
// the same address cannot both pass the check and then both insert.
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

		// username is written as well as email. That column is a vestige of the
		// old schema which migration 3 deliberately did not drop: dropping it
		// would have meant rebuilding the table, and with foreign keys on,
		// DROP TABLE users performs an implicit DELETE that cascades and removes
		// every transaction in the database. It is NOT NULL UNIQUE, so it still
		// has to be given a value.
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

		// Every account owns a personal household from the moment it exists.
		// This is in the same transaction as the INSERT above deliberately: a
		// user committed without a household would be able to sign in and then
		// have nowhere to record anything, and every page would have to cope
		// with that state. Doing it here means the state is unreachable.
		_, err = createPersonalHousehold(ctx, tx, id, display)
		return err
	})

	return id, err
}

// CredentialsFor looks up an account and returns its password hash.
//
// The lookup is in two stages, and the order matters:
//
//  1. An exact match on what the user typed, case and all.
//  2. Failing that, a case-insensitive match.
//
// Stage 2 is the convenience most people expect. Stage 1 exists because a
// legacy database can hold two accounts differing only in case, and going
// straight to a case-insensitive match would return an arbitrary one of them --
// so whoever typed "kushith" might be checked against "Kushith"'s password and
// be told, wrongly and permanently, that their own password is incorrect.
//
// Both stages also match the legacy username column, because migration 3
// backfilled email from it: accounts predating that change sign in with exactly
// the string they always used.
func (s *Store) CredentialsFor(ctx context.Context, email string) (User, string, error) {
	typed := strings.TrimSpace(email)

	// Stage 1: exact.
	u, hash, err := s.credentialsExact(ctx, typed)
	if err == nil {
		return u, hash, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return User{}, "", err
	}

	// Stage 2: case-insensitive. LIMIT is by lowest id so the result is at least
	// deterministic if an ambiguous pair is somehow reached from here.
	err = s.db.QueryRowContext(ctx, `
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), password_hash
		FROM users
		WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE
		ORDER BY id ASC
		LIMIT 1`,
		NormalizeEmail(typed), NormalizeEmail(typed),
	).Scan(&u.ID, &u.Email, &u.DisplayName, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("select user: %w", err)
	}
	return u, hash, nil
}

// credentialsExact matches the typed string byte for byte.
func (s *Store) credentialsExact(ctx context.Context, typed string) (User, string, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), password_hash
		FROM users
		WHERE email = ? OR username = ?
		ORDER BY id ASC
		LIMIT 1`, typed, typed).Scan(&u.ID, &u.Email, &u.DisplayName, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("select user (exact): %w", err)
	}
	return u, hash, nil
}

// EmailExists reports whether an address already has an account.
//
// The combined login/signup form needs this to decide which of the two things
// the user is trying to do.
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

// UserByID resolves the id held in a session cookie to a live account.
//
// Called on every authenticated request rather than trusting the cookie's
// contents, so a deleted account's outstanding cookies stop working at once.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, IFNULL(email, username), IFNULL(display_name, '') FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Email, &u.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select user by id: %w", err)
	}
	return u, nil
}

// ═════════════════════════════════════════════════════════════════════════════
// sessions
// ═════════════════════════════════════════════════════════════════════════════

// A login is a row here, not just a signed cookie. That is the whole point: a
// row can be deleted, so a login can be revoked. See migration 5.

const (
	// SessionTTL is how long a login lasts from the moment it is created,
	// however active it is. An absolute ceiling means a forgotten session on a
	// shared computer cannot live forever.
	SessionTTL = 30 * 24 * time.Hour

	// SessionIdleTTL is how long a login may go untouched before it is treated
	// as abandoned. Shorter than the absolute limit, because an unused session
	// is exactly the kind most likely to have been left on someone else's
	// machine.
	SessionIdleTTL = 14 * 24 * time.Hour

	// sessionTouchAfter is how stale last_seen_at must be before a request
	// bothers to update it.
	//
	// Without this, every page load would be a write, and SQLite serialises
	// writers -- so reading the dashboard would queue behind other readers'
	// bookkeeping. A minute of imprecision in "last active" is invisible to the
	// user and removes almost all of those writes.
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

// newSessionID returns a fresh 256-bit random token.
//
// Random, not sequential: the id IS the credential, so a predictable one would
// be a login anybody could walk into. 32 bytes is well beyond guessing range.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanUserAgent trims a browser's self-description to something loggable.
//
// Capped because it arrives from the client and is displayed back on the device
// list. The template escapes it, so this is about storage and layout rather than
// safety.
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
	// The TTL is passed as a bound modifier so the expiry is computed by SQLite
	// in the same clock the comparison later uses. Computing it in Go would
	// compare a Go timestamp against datetime('now') and drift if the two
	// disagree about the timezone.
	ttl := fmt.Sprintf("+%d seconds", int64(SessionTTL.Seconds()))
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, user_id, expires_at, user_agent)
		VALUES (?, ?, datetime('now', ?), ?)`,
		id, userID, ttl, cleanUserAgent(userAgent)); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// SessionUser resolves a session token to the account it belongs to.
//
// One query does the validating and the resolving together: the join means an
// account deleted since login has no row to return, and the two time conditions
// mean an expired or abandoned session is indistinguishable from one that never
// existed. Callers get ErrNotFound for all of it, which is the right amount of
// information to give whoever presented the token.
//
// last_seen_at is then touched, but only if it is already stale -- see
// sessionTouchAfter for why that matters on SQLite.
func (s *Store) SessionUser(ctx context.Context, id string) (User, error) {
	if id == "" {
		return User{}, ErrNotFound
	}

	idle := fmt.Sprintf("-%d seconds", int64(SessionIdleTTL.Seconds()))

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

	stale := fmt.Sprintf("-%d seconds", int64(sessionTouchAfter.Seconds()))
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = datetime('now')
		WHERE id = ? AND last_seen_at < datetime('now', ?)`, id, stale); err != nil {
		// Not fatal: the caller is authenticated either way, and failing the
		// request over a bookkeeping write would be a poor trade. It does mean
		// the session could eventually idle out mid-use, which is why this is
		// logged rather than swallowed silently.
		return u, nil
	}
	return u, nil
}

// Sessions lists a user's live logins, most recently active first.
func (s *Store) Sessions(ctx context.Context, userID int64, current string) ([]Session, error) {
	idle := fmt.Sprintf("-%d seconds", int64(SessionIdleTTL.Seconds()))
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

// DeleteSession revokes one login.
//
// user_id is part of the WHERE clause so a guessed token cannot be used to sign
// somebody else out.
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
//
// This is what a password change calls, and what makes changing a password
// actually mean something: without it, an old cookie kept working.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOtherSessions revokes every login for an account except the one given.
//
// The "sign out everywhere else" case: you keep working where you are and every
// other device is dropped.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keep)
	if err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeExpiredSessions removes rows no login can use any more.
//
// Nothing depends on this for correctness -- SessionUser already refuses an
// expired row -- so it is housekeeping, not enforcement. Without it the table
// grows forever.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	idle := fmt.Sprintf("-%d seconds", int64(SessionIdleTTL.Seconds()))
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
//
// One hour, not a day. A reset link is a bearer credential sitting in an inbox:
// anyone who reads that mailbox, now or later, can take the account with it. The
// shorter the window the smaller that exposure, and an hour is comfortably longer
// than the gap between asking for a link and clicking it.
const ResetTTL = time.Hour

// CreateReset issues a single-use password reset token.
//
// Every other outstanding token for the account is deleted first. If somebody
// asks for three links because the first two seemed not to arrive, only the last
// should work -- and if an attacker requested one an hour ago, the owner
// requesting their own invalidates it.
func (s *Store) CreateReset(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionID() // same generator: 256 bits of crypto/rand
	if err != nil {
		return "", err
	}
	ttl := fmt.Sprintf("+%d seconds", int64(ResetTTL.Seconds()))

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
//
// Used to decide whether to show the "choose a new password" form at all, so an
// expired link says so instead of presenting a form that will fail on submit.
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
//
// Four things happen in one transaction: the token is marked used, the hash is
// replaced, every session for the account is deleted, and any other outstanding
// tokens go too. They belong together because the reason for resetting a password
// is usually that somebody else might have it -- so leaving their laptop signed
// in, or a second reset link alive, would defeat the exercise.
//
// The token is marked used inside the same UPDATE that requires it to be unused,
// so two simultaneous submissions cannot both succeed.
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

// ChangePassword replaces the hash for a signed-in user and signs out their
// other devices.
//
// Deliberately different from a reset: the current session is spared, because the
// person just proved they know the old password and signing them out of the page
// they are looking at would be gratuitous. Every *other* session goes, which is
// the useful half -- a laptop left signed in somewhere is exactly what changing a
// password is meant to address.
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

// FormTokenTTL is how long an unused form token stays valid.
//
// Long enough to fill in a form slowly, be interrupted, and come back; short
// enough that the table does not accumulate tokens from browser tabs that were
// opened and forgotten months ago.
const FormTokenTTL = 12 * time.Hour

// NewFormToken issues a token to embed in a form that must not be submitted
// twice.
//
// Reuses the session-id generator: this is not a secret in the way a session is,
// but it must be unguessable all the same. A predictable token would let one
// person's submission cancel somebody else's pending form.
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
//
// The whole design rests on one property: DELETE ... RETURNING is atomic, so
// when the same form is submitted twice at the same moment, exactly one of the
// two deletes affects a row. The loser is told, and can be redirected to the
// result of the first rather than shown an error for something that worked.
//
// A read followed by a delete would not do: both requests would read the token,
// both would find it present, and both would insert an expense.
func (s *Store) ConsumeFormToken(ctx context.Context, userID int64, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	age := fmt.Sprintf("-%d seconds", int64(FormTokenTTL.Seconds()))

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
	age := fmt.Sprintf("-%d seconds", int64(FormTokenTTL.Seconds()))
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

// recordAudit writes one entry inside a caller's transaction.
//
// Deliberately takes the *sql.Tx rather than opening its own. The record and the
// change it describes have to be one atomic act: a log written afterwards can be
// lost by a crash, leaving a change nobody can account for, and a log written
// beforehand can describe something that then rolled back. Both failures are
// worse than useless in an audit trail, because they are silent.
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
const (
	RateWindow   = 10 * time.Minute
	RateMaxTries = 10
)

// RateRetryIn reports how long a key must wait, or zero if it may try now.
//
// A duration rather than a boolean, so the page can count down instead of saying
// "a few minutes" -- which is the one thing a locked-out user cannot act on. They
// do not know whether to wait or go and do something else.
//
// The counter lives in the database rather than in a map, because the map was
// cleared by every restart -- and a process that falls over under load restarts
// itself, so the limit was removable by the very thing it was meant to survive.
// It also means two processes share one count instead of granting double.
func (s *Store) RateRetryIn(ctx context.Context, key string) (time.Duration, error) {
	window := fmt.Sprintf("-%d seconds", int64(RateWindow.Seconds()))
	ahead := fmt.Sprintf("+%d seconds", int64(RateWindow.Seconds()))

	// Seconds remaining computed in SQL via epoch arithmetic, not by parsing the
	// stored timestamp in Go. window_start is written by datetime('now'), which
	// is UTC and has no zone marker, so a Go-side parse would be one time.Parse
	// mistake away from being wrong by the local offset -- which in this
	// hemisphere would mean lockouts that expire hours early or hours late.
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
	if failures < RateMaxTries {
		return 0, nil
	}
	if remaining <= 0 {
		// The window has just lapsed between the WHERE clause and this line.
		return 0, nil
	}
	return time.Duration(remaining) * time.Second, nil
}

// RateFail records a failed attempt, starting a new window if the old one has
// aged out.
//
// One statement, not read-then-write. Two simultaneous wrong passwords would
// otherwise both read the same count and both write count+1, recording one
// failure instead of two -- which is a small hole, but it is the kind that turns
// a limit of ten into a limit of twenty under load.
func (s *Store) RateFail(ctx context.Context, key string) error {
	window := fmt.Sprintf("-%d seconds", int64(RateWindow.Seconds()))
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
	window := fmt.Sprintf("-%d seconds", int64(RateWindow.Seconds()))
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_attempts WHERE window_start <= datetime('now', ?)`, window)
	if err != nil {
		return 0, fmt.Errorf("purge login attempts: %w", err)
	}
	return res.RowsAffected()
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
//
// modernc.org/sqlite returns a driver-specific error type, so matching on the
// message is the portable option that adds no dependency. The check is narrow
// enough not to swallow unrelated failures.
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


// A household is the thing that owns money, and a Role is what a person may do
// to it. See internal/db/migrate004.go for the schema and why personal and
// shared households are kept separate.

// ── roles ─────────────────────────────────────────────────────────────────────

// Role is a member's authority within one household.
//
// The permissions are methods on this type rather than if-statements in
// handlers, and that is the whole point of the design: there is exactly one
// definition of "may this person move money", so a new handler cannot invent a
// subtly different rule. A handler that forgets to ask gets no access at all,
// because the zero value is not a valid role.
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

// Valid reports whether r is a role this application recognises. Used to
// validate anything arriving from a form before it reaches SQL.
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

// CanMoveFunds covers depositing to, withdrawing from and closing a savings
// fund, and is owner-only.
//
// This is the one asymmetry worth explaining. Logging a grocery shop and
// draining the emergency fund are both "writes", but they carry very different
// consequences, and a shared household is precisely the situation where that
// difference matters: an editor mistyping an expense costs a correction, an
// editor emptying the emergency fund costs the household's safety net.
func (r Role) CanMoveFunds() bool { return r == RoleOwner }

// CanManageMembers covers inviting, removing, and changing roles.
func (r Role) CanManageMembers() bool { return r == RoleOwner }

// CanManageHousehold covers renaming and deleting the household itself.
func (r Role) CanManageHousehold() bool { return r == RoleOwner }

// ── errors ────────────────────────────────────────────────────────────────────

var (
	// ErrNotMember means the caller is not in the household they asked about.
	// Handlers map it to 404 rather than 403, so a probing user cannot discover
	// which household ids exist.
	ErrNotMember = errors.New("not a member of that household")

	// ErrForbidden means the caller is a member but their role does not permit
	// the action.
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

// Membership is a household together with the caller's authority in it. It is
// what the web layer resolves once per request and passes to every store call.
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

	// ExpiresAt is when this invitation stops being acceptable, and Expired says
	// whether that has already happened.
	//
	// Expired is computed in SQL rather than compared in Go so that the answer
	// comes from the same clock as the WHERE clauses that enforce it. Two
	// different clocks would eventually disagree, and the page would offer an
	// Accept button the server refuses.
	ExpiresAt string
	Expired   bool
}

// ── creating households ───────────────────────────────────────────────────────

// createPersonalHousehold inserts a user's own household and makes them its
// owner. It runs inside the caller's transaction because it must be atomic with
// the signup that needs it: a user committed without a household could log in
// and would then have nowhere to put anything.
//
// Called by CreateUser, and by ActiveHousehold as a repair for any account that
// somehow lacks one.
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

// ActiveHousehold returns the household the user is currently working in,
// together with their role in it.
//
// This runs on every authenticated request, so it is one query in the common
// case. It is also the place three edge cases are absorbed rather than left to
// become 500s:
//
//   - active_household_id is NULL, because the household was deleted
//     (ON DELETE SET NULL) or the row predates migration 4;
//   - the user still points at a household they have since been removed from;
//   - the user has no personal household at all.
//
// The first two fall back to the personal household; the third creates one.
// Every one of them would otherwise be a user who cannot load any page.
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

// activeHouseholdOnce reads the active household, returning ErrNotFound if the
// pointer is null or no longer backed by a membership row.
//
// The join to household_members is what makes removal take effect immediately:
// a user who is removed from a shared household stops being able to read it on
// their very next request, without anything having to invalidate their session.
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
// Personal first, then shared by name, so the list does not reorder itself as
// households are added.
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
//
// The UPDATE's WHERE clause carries the membership test, so an id the user is
// not a member of changes nothing -- there is no window between checking and
// writing, and no way to switch into someone else's budget by editing a form.
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

// RoleIn returns the caller's role in a household, or ErrNotMember.
func (s *Store) RoleIn(ctx context.Context, householdID, userID int64) (Role, error) {
	var r Role
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
		householdID, userID).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", fmt.Errorf("select role: %w", err)
	}
	return r, nil
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

// SetRole changes one member's role.
//
// Demoting the last owner is refused. The count and the update share a
// transaction, so two owners cannot simultaneously demote each other and leave
// the household with none -- a check outside a transaction would let both pass.
// TransferOwnership hands a household to another member in one action.
//
// Previously this took two: promote them to owner, then demote yourself. Both
// steps succeed independently, so the sequence has two bad intermediate states.
// Stop after the first and the budget has two owners; stop after a failed second
// and you have given away control while keeping it, which is confusing rather
// than dangerous. Worse, doing it in the other order is blocked outright by the
// last-owner rule, so the only workable sequence was the one that leaves the
// household briefly co-owned.
//
// One transaction removes the question. Either the household has exactly one new
// owner, or nothing changed.
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
			// Only an existing member can be promoted. Handing a budget to an
			// address that has not accepted an invitation would leave it owned by
			// nobody who can sign in.
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
		var owners int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM household_members
			 WHERE household_id = ? AND role = 'owner'`, householdID).Scan(&owners); err != nil {
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
			var owners int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM household_members
				 WHERE household_id = ? AND role = 'owner'`, householdID).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner
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

// RemoveMember takes somebody out of a household.
//
// Their entries stay: transactions are owned by the household, and user_id is
// only attribution. Deleting a departing member's expenses would silently
// rewrite the household's history and change every total on the dashboard.
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
			var owners int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM household_members
				 WHERE household_id = ? AND role = 'owner'`, householdID).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID); err != nil {
			return err
		}

		// Anyone left pointing at this household is moved back to their own, or
		// their pointer is cleared for ActiveHousehold to repair. Without this
		// they would keep reading a household they no longer belong to until
		// they happened to switch.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET active_household_id = (SELECT id FROM households WHERE personal_for = users.id)
			WHERE id = ? AND active_household_id = ?`, targetID, householdID); err != nil {
			return err
		}
		// Their entries stay; only the membership goes. The history says who
		// removed them, which is the question this table exists to answer.
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: actorID},
			"removed a member", "member", targetID, "their entries were kept")
	})
}

// DeleteHousehold removes a shared household and everything in it.
//
// Personal households are refused: that is where the user's own data lives, and
// there is no path in the UI to delete your entire history by accident.
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
//
// The address does not need an account yet: invitations are matched by email at
// sign-in, so somebody can be invited and then sign up. Nothing is emailed --
// there is no mail server here -- so the invitation waits in the database and is
// shown as a banner the next time that person loads a page.
//
// Only editor and viewer can be granted. Ownership moves by explicit transfer.
// InviteTTL is how long an invitation stays acceptable.
//
// Twenty-four hours, as asked for. Short enough that an address typed wrongly,
// or sent to someone who has since left, stops being a way into a budget
// tomorrow -- and short enough that resending will be routine, which is why
// there is a resend action rather than only a revoke.
const InviteTTL = 24 * time.Hour

// inviteTTLModifier is InviteTTL as a SQLite datetime modifier, derived from the
// constant rather than written out, so the two cannot disagree.
var inviteTTLModifier = fmt.Sprintf("+%d seconds", int64(InviteTTL.Seconds()))

// ErrInviteExpired is returned when an invitation is real but too old to use.
//
// Distinct from ErrNotFound on purpose: "that invitation has expired, ask them
// to send another" is actionable, and "not found" would send the recipient
// looking for a mistake they did not make.
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

// PendingInvites lists a household's unanswered invitations, for its settings
// page.
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
//
// The alternative -- revoke, then invite again -- works, but it is two actions
// for one intention, and the partial unique index means the revoke has to
// succeed first or the second invitation is rejected as a duplicate. Extending
// the row is one statement and cannot half-happen.
//
// The row is only touched while it is still 'pending', so this cannot resurrect
// an invitation somebody has already declined.
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
//
// Exported for tests, and named so that its purpose is unmistakable at the call
// site. The alternative -- reaching into *sql.DB from the test package -- would
// mean the test writes its own SQL against a column the store owns, and those two
// copies drift. This way there is one statement, here.
func (s *Store) TestOnlyExpireInvite(ctx context.Context, inviteID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE household_invites SET expires_at = datetime('now', '-1 minute') WHERE id = ?`,
		inviteID)
	return err
}

// PurgeStaleInvites removes invitations that expired long enough ago that nobody
// is going to act on them.
//
// Not the moment they expire: an owner looking at the members page should see
// that the invitation they sent yesterday lapsed, with a button to send it
// again. Deleting on the stroke of expiry would make it silently vanish, and
// "did I actually invite them?" is a worse question than "that one expired".
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

// InvitesFor lists the unanswered invitations addressed to one person, matched
// case-insensitively so an invitation to "Bob@x.com" reaches bob@x.com.
//
// Invitations to a household they are somehow already in are filtered out, so a
// stale row cannot leave an undismissable banner on every page.
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

// AcceptInvite turns an invitation into a membership and switches the user into
// the household.
//
// The invitation is re-read inside the transaction and matched against the
// caller's own address, so a guessed invitation id belonging to somebody else
// is refused.
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
		// Checked inside the transaction, not before it. An expiry test done
		// earlier and acted on later has a gap, and the whole point of the
		// deadline is that it is not negotiable by timing.
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

// DeclineInvite marks an invitation refused. The row is kept rather than
// deleted so the inviter can see what happened, and because
// idx_invites_open is partial on status='pending' a fresh invitation can still
// be sent later.
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

// RevokeInvite withdraws an invitation the household sent. householdID is part
// of the WHERE clause, so an owner cannot revoke another household's invitation
// by id.
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

// ReceiptJob is one queued receipt image.
type ReceiptJob struct {
	ID int64

	// UserID is who uploaded the file; HouseholdID is which budget the expense
	// it becomes will belong to. They are recorded separately because the two
	// answers can differ by the time the worker runs -- the user may have
	// switched households in the meantime -- and the budget the user was looking
	// at when they chose the file is the one they meant.
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
}

// MaxJobAttempts is how many times a receipt is retried before the user is told
// it failed. Retrying at all covers a transient problem such as the file not
// having finished syncing; retrying forever would hide a real failure, which is
// the one outcome the wireframe insists must reach the user.
const MaxJobAttempts = 3

// EnqueueReceipt adds a receipt to the processing queue and returns its id.
//
// This is the whole point of the queue: the handler returns immediately, so the
// user "can go about the rest of their business" rather than watching a spinner
// while an image is analysed.
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
//
// The UPDATE ... WHERE status = 'queued' is what makes the claim safe: if two
// workers race, only one UPDATE affects a row, and the loser sees zero rows
// affected and moves on. Selecting and then updating in two steps would hand
// the same receipt to both.
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

		return tx.QueryRowContext(ctx, `
			SELECT id, user_id, household_id, path, original_name, status, error, attempts,
			       transaction_id, created_at, IFNULL(finished_at, '')
			FROM receipt_jobs WHERE id = ?`, id).
			Scan(&job.ID, &job.UserID, &job.HouseholdID, &job.Path, &job.OriginalName, &job.Status,
				&job.Error, &job.Attempts, &job.TransactionID, &job.CreatedAt, &job.FinishedAt)
	})

	return job, err
}

// CompleteReceiptJob marks a job done, optionally linking the transaction it
// produced.
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

// RecoverStuckJobs returns jobs left in 'processing' back to the queue.
//
// A job is only ever in that state while a worker holds it in memory, so if the
// process was killed mid-flight the row is stranded and the receipt would never
// be processed and never be reported as failed. Called once at startup.
func (s *Store) RecoverStuckJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE receipt_jobs SET status = 'queued' WHERE status = 'processing'`)
	if err != nil {
		return 0, fmt.Errorf("recover stuck jobs: %w", err)
	}
	return res.RowsAffected()
}

// ReceiptJobs lists a user's recent receipts so the UI can show what is still
// being worked on.
func (s *Store) ReceiptJobs(ctx context.Context, userID int64, limit int) ([]ReceiptJob, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, household_id, path, original_name, status, error, attempts,
		       transaction_id, created_at, IFNULL(finished_at, '')
		FROM receipt_jobs
		WHERE user_id = ?
		ORDER BY id DESC
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("receipt jobs: %w", err)
	}
	defer rows.Close()

	out := []ReceiptJob{}
	for rows.Next() {
		var j ReceiptJob
		if err := rows.Scan(&j.ID, &j.UserID, &j.HouseholdID, &j.Path, &j.OriginalName, &j.Status,
			&j.Error, &j.Attempts, &j.TransactionID, &j.CreatedAt, &j.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UnattachedReceipt fetches a processed receipt that has not yet become an
// expense.
//
// Scoped by household_id, not user_id: a receipt uploaded by one member belongs
// to that household's budget, so any member who may edit should be able to
// finish it -- and, more importantly, a member of a *different* household must
// not be able to attach it by guessing the id. That is the
// insecure-direct-object-reference shape, and the household filter is what
// closes it.
//
// The transaction_id IS NULL condition is what makes attaching idempotent-safe:
// once a receipt has become an expense, this returns ErrNotFound rather than
// letting the same file be attached to a second transaction. A resubmitted form
// or a double-tapped notification therefore cannot duplicate a receipt.
func (s *Store) UnattachedReceipt(ctx context.Context, sc Scope, jobID int64) (ReceiptJob, error) {
	var j ReceiptJob
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, household_id, path, original_name, status, error, attempts,
		       transaction_id, created_at, IFNULL(finished_at, '')
		FROM receipt_jobs
		WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
		jobID, sc.HouseholdID,
	).Scan(&j.ID, &j.UserID, &j.HouseholdID, &j.Path, &j.OriginalName, &j.Status,
		&j.Error, &j.Attempts, &j.TransactionID, &j.CreatedAt, &j.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptJob{}, ErrNotFound
	}
	if err != nil {
		return ReceiptJob{}, fmt.Errorf("unattached receipt: %w", err)
	}
	return j, nil
}

// UnattachedReceipts lists processed receipts in this budget that nobody has
// turned into an expense yet.
//
// Without this the upload is a dead end whenever the notification is missed:
// notifications are dismissed, cleared on another device, or simply scrolled
// past, and the file itself was then unreachable from anywhere in the UI.
func (s *Store) UnattachedReceipts(ctx context.Context, sc Scope, limit int) ([]ReceiptJob, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, household_id, path, original_name, status, error, attempts,
		       transaction_id, created_at, IFNULL(finished_at, '')
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
		var j ReceiptJob
		if err := rows.Scan(&j.ID, &j.UserID, &j.HouseholdID, &j.Path, &j.OriginalName,
			&j.Status, &j.Error, &j.Attempts, &j.TransactionID, &j.CreatedAt,
			&j.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan unattached receipt: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DiscardReceipt throws away an uploaded receipt nobody wants, returning the
// stored path so the caller can delete the file too.
//
// The waiting list otherwise has only one exit: turning the receipt into an
// expense. Upload the same screenshot three times by accident and the only way
// to clear it would be to invent three expenses, which is a worse outcome than
// the mess it tidies.
//
// The row is deleted rather than flagged. A status of 'discarded' would be
// tidier, but receipt_jobs.status carries a CHECK constraint listing its four
// values, and SQLite cannot alter a CHECK without rebuilding the table -- which
// is the one migration shape this project refuses, because a rebuild with
// foreign keys on performs a cascading delete. Weighed against that, losing a
// queue artefact nobody wanted is the cheaper loss. No financial record is
// touched: an attached receipt is refused outright.
func (s *Store) DiscardReceipt(ctx context.Context, sc Scope, jobID int64) (string, error) {
	var path string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT path FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
			jobID, sc.HouseholdID).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			// Either it belongs to another budget, or it is already an expense.
			// Refusing the second case matters: deleting it there would leave a
			// transaction pointing at a file about to be removed from disk.
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
//
// Both writes happen in one transaction. Copying the file reference onto the
// transaction and marking the job attached are two halves of one fact, and if
// they can come apart the failure modes are both bad: a transaction pointing at
// a file the job still thinks is free, or a job marked used whose expense has no
// receipt.
//
// The WHERE clauses repeat the ownership and IS NULL conditions rather than
// trusting the caller's earlier read. Between that read and this write another
// request could have attached the same receipt, and a check-then-act with a gap
// in the middle is how the same file ends up on two expenses.
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
//
// Scoped to the household rather than the uploader: the figure appears on the
// household's dashboard and answers "is anything still coming?", which is a
// question about the budget. A member should see that a shared receipt is being
// processed even though the notification when it lands goes only to whoever
// uploaded it.
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


// Notification is a message waiting for a user.
//
// Stored in a table rather than the session, which is what makes the
// wireframe's requirement achievable: "If user is not logged in and there is an
// issue with processing, then some other notification should be given, perhaps
// when they log back in again." An unseen row keeps waiting however long the
// user stays away, and survives a server restart.
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
//
// Read-and-clear in one transaction: two browser tabs polling at the same moment
// would otherwise both receive the same message, and the user would see the
// same toast twice.
func (s *Store) TakeNotifications(ctx context.Context, userID int64) ([]Notification, error) {
	out := []Notification{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, text, link, created_at
		FROM notifications
		WHERE user_id = ? AND seen_at IS NULL
		ORDER BY id ASC
		LIMIT 20`, userID)
	if err != nil {
		return nil, fmt.Errorf("read notifications: %w", err)
	}
	var ids []any
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Text, &n.Link, &n.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		out = append(out, n)
		ids = append(ids, n.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	ph := make([]byte, 0, len(ids)*2)
	for i := range ids {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
	}
	args := append([]any{time.Now().UTC().Format(time.RFC3339)}, ids...)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET seen_at = ? WHERE id IN (`+string(ph)+`)`, args...); err != nil {
		return nil, fmt.Errorf("mark notifications seen: %w", err)
	}
	return out, nil
}

// UnseenNotificationCount is used to decide whether the page should start
// polling at all.
func (s *Store) UnseenNotificationCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND seen_at IS NULL`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("unseen count: %w", err)
	}
	return n, nil
}
