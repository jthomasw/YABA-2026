package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

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
	// A variable bucket needs its usual amount (the trailing average) until
	// this month's spending passes it, and then whatever was actually spent.
	//
	// It used to switch to the actual spend as soon as anything was entered,
	// so a $120 power bill with a $5 charge recorded on the 2nd counted as
	// needing $5 for the month and showed as funded -- and the rest of the
	// month's income was poured into lower priorities ahead of it.
	return max(b.Spent, b.Estimate)
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
