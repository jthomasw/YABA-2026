package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

type RecurringIncome struct {
	ID            int64
	HouseholdID   int64
	UserID        int64
	Source        string
	Amount        Cents
	FrequencyN    int
	FrequencyUnit string
	StartDate     string
	NextDueDate   string
	Active        bool
	CreatedAt     string
	UpdatedAt     string
}

func (s *Store) CreateRecurringIncome(
	ctx context.Context,
	sc Scope,
	source string,
	amount Cents,
	frequencyN int,
	frequencyUnit string,
	startDate string,
) (int64, error) {
	source = cleanLabel(source)

	if source == "" {
		return 0, fmt.Errorf("recurring income needs a source")
	}

	if amount <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}

	if ok, msg := ValidFrequency(frequencyN, frequencyUnit); !ok {
		return 0, fmt.Errorf("invalid recurring frequency: %s", msg)
	}

	startDate, err := ParseDate(startDate)
	if err != nil {
		return 0, err
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO recurring_income (
			household_id,
			user_id,
			source,
			amount_cents,
			frequency_n,
			frequency_unit,
			start_date,
			next_due_date
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.HouseholdID,
		sc.UserID,
		source,
		int64(amount),
		frequencyN,
		frequencyUnit,
		startDate,
		startDate,
	)
	if err != nil {
		return 0, fmt.Errorf("create recurring income: %w", err)
	}

	return res.LastInsertId()
}

func (s *Store) ListRecurringIncome(
	ctx context.Context,
	sc Scope,
) ([]RecurringIncome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id,
			household_id,
			user_id,
			source,
			amount_cents,
			frequency_n,
			frequency_unit,
			start_date,
			next_due_date,
			active,
			created_at,
			updated_at
		FROM recurring_income
		WHERE household_id = ?
		ORDER BY active DESC, id ASC`,
		sc.HouseholdID,
	)
	if err != nil {
		return nil, fmt.Errorf("list recurring income: %w", err)
	}
	defer rows.Close()

	out := []RecurringIncome{}

	for rows.Next() {
		var r RecurringIncome
		var amount int64
		var active int64

		if err := rows.Scan(
			&r.ID,
			&r.HouseholdID,
			&r.UserID,
			&r.Source,
			&amount,
			&r.FrequencyN,
			&r.FrequencyUnit,
			&r.StartDate,
			&r.NextDueDate,
			&active,
			&r.CreatedAt,
			&r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recurring income: %w", err)
		}

		r.Amount = Cents(amount)
		r.Active = active == 1

		out = append(out, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recurring income: %w", err)
	}

	return out, nil
}

func (s *Store) RecurringIncomeByID(
	ctx context.Context,
	sc Scope,
	id int64,
) (RecurringIncome, error) {
	var r RecurringIncome
	var amount int64
	var active int64

	err := s.db.QueryRowContext(ctx, `
		SELECT
			id,
			household_id,
			user_id,
			source,
			amount_cents,
			frequency_n,
			frequency_unit,
			start_date,
			next_due_date,
			active,
			created_at,
			updated_at
		FROM recurring_income
		WHERE id = ? AND household_id = ?`,
		id,
		sc.HouseholdID,
	).Scan(
		&r.ID,
		&r.HouseholdID,
		&r.UserID,
		&r.Source,
		&amount,
		&r.FrequencyN,
		&r.FrequencyUnit,
		&r.StartDate,
		&r.NextDueDate,
		&active,
		&r.CreatedAt,
		&r.UpdatedAt,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return RecurringIncome{}, ErrNotFound
	}
	if err != nil {
		return RecurringIncome{}, fmt.Errorf("get recurring income: %w", err)
	}

	r.Amount = Cents(amount)
	r.Active = active == 1

	return r, nil
}

func (s *Store) UpdateRecurringIncome(
	ctx context.Context,
	sc Scope,
	id int64,
	source string,
	amount Cents,
	frequencyN int,
	frequencyUnit string,
	startDate string,
	nextDueDate string,
) error {
	source = cleanLabel(source)

	if source == "" {
		return fmt.Errorf("recurring income needs a source")
	}

	if amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	// The same ceilings CreateRecurringIncome applies. Checking only "> 0"
	// here let an edit store an interval the catch-up loop cannot advance.
	if ok, msg := ValidFrequency(frequencyN, frequencyUnit); !ok {
		return fmt.Errorf("invalid recurring frequency: %s", msg)
	}

	startDate, err := ParseDate(startDate)
	if err != nil {
		return err
	}

	nextDueDate, err = ParseDate(nextDueDate)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE recurring_income
		SET
			source = ?,
			amount_cents = ?,
			frequency_n = ?,
			frequency_unit = ?,
			start_date = ?,
			next_due_date = ?,
			updated_at = datetime('now')
		WHERE id = ? AND household_id = ? AND active = 1`,
		source,
		int64(amount),
		frequencyN,
		frequencyUnit,
		startDate,
		nextDueDate,
		id,
		sc.HouseholdID,
	)
	if err != nil {
		return fmt.Errorf("update recurring income: %w", err)
	}

	return requireOneRow(res)
}

func (s *Store) SetRecurringIncomeActive(
	ctx context.Context,
	sc Scope,
	id int64,
	active bool,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE recurring_income
		SET
			active = ?,
			updated_at = datetime('now')
		WHERE id = ? AND household_id = ?`,
		boolToInt(active),
		id,
		sc.HouseholdID,
	)
	if err != nil {
		return fmt.Errorf("set recurring income active: %w", err)
	}

	return requireOneRow(res)
}

func (s *Store) DeleteRecurringIncome(
	ctx context.Context,
	sc Scope,
	id int64,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM recurring_income
		WHERE id = ? AND household_id = ?`,
		id,
		sc.HouseholdID,
	)
	if err != nil {
		return fmt.Errorf("delete recurring income: %w", err)
	}

	return requireOneRow(res)
}

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

func (s *Store) ProcessDueRecurringIncome(
	ctx context.Context,
	sc Scope,
	asOf string,
) error {
	asOf, err := ParseDate(asOf)
	if err != nil {
		return err
	}

	schedules, err := s.ListRecurringIncome(ctx, sc)
	if err != nil {
		return err
	}

	// Months that gained a transaction, re-poured at the end so the funding
	// waterfall reflects the income that just arrived.
	touched := map[string]bool{}

	for _, r := range schedules {
		if !r.Active {
			continue
		}

		// (A date drifted by the old month arithmetic was repaired once, by
		// migration 19, rather than here on every run.)
		nextDue := r.NextDueDate

		// Bounded: see maxCatchUpOccurrences. Anything still owed after that
		// is generated on the next visit, because next_due_date only ever
		// advances as far as the work actually done.
		for made := 0; nextDue <= asOf && made < maxCatchUpOccurrences; made++ {
			err := s.inTx(ctx, func(tx *sql.Tx) error {
				var existing int64
				err := tx.QueryRowContext(ctx, `
					SELECT transaction_id
					FROM recurring_income_occurrences
					WHERE recurring_income_id = ? AND due_date = ?`,
					r.ID,
					nextDue,
				).Scan(&existing)

				if err == nil {
					return nil
				}

				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("check recurring income occurrence: %w", err)
				}

				res, err := tx.ExecContext(ctx, `
					INSERT INTO transactions (
						household_id,
						user_id,
						kind,
						label,
						amount_cents,
						occurred_on
					)
					VALUES (?, ?, 'income', ?, ?, ?)`,
					sc.HouseholdID,
					r.UserID,
					cleanLabel(r.Source),
					int64(r.Amount),
					nextDue,
				)
				if err != nil {
					return fmt.Errorf("create recurring paycheck transaction: %w", err)
				}

				transactionID, err := res.LastInsertId()
				if err != nil {
					return err
				}

				_, err = tx.ExecContext(ctx, `
					INSERT INTO recurring_income_occurrences (
						recurring_income_id,
						due_date,
						transaction_id
					)
					VALUES (?, ?, ?)`,
					r.ID,
					nextDue,
					transactionID,
				)
				if err != nil {
					return fmt.Errorf("record recurring income occurrence: %w", err)
				}

				touched[nextDue[:7]] = true

				// Attributed to whoever set the schedule up, not to whoever
				// happened to trigger the catch-up (or to nobody, when the
				// background runner did).
				return recordAudit(
					ctx,
					tx,
					Scope{HouseholdID: sc.HouseholdID, UserID: r.UserID},
					"created",
					"transaction",
					transactionID,
					fmt.Sprintf(
						"Recurring income %s — %q on %s",
						r.Amount.Display(),
						cleanLabel(r.Source),
						nextDue,
					),
				)
			})
			if err != nil {
				return err
			}

			nextDue, err = advanceRecurringDate(
				r.StartDate,
				nextDue,
				r.FrequencyN,
				r.FrequencyUnit,
			)
			if err != nil {
				return err
			}
		}

		if nextDue != r.NextDueDate {
			_, err := s.db.ExecContext(ctx, `
				UPDATE recurring_income
				SET
					next_due_date = ?,
					updated_at = datetime('now')
				WHERE id = ? AND household_id = ?`,
				nextDue,
				r.ID,
				sc.HouseholdID,
			)
			if err != nil {
				return fmt.Errorf("advance recurring income: %w", err)
			}
		}
	}

	s.reallocateTouched(ctx, sc, touched)
	return nil
}

// ── recurring expenses ───────────────────────────────────────────────────────
//
// The same machine as recurring income above, for money going the other way.
// Deliberately a parallel implementation rather than a shared generic one: the
// two differ in what a generated transaction carries (an expense has a bucket
// and an essential flag, income has neither) and in nothing else, and a single
// abstraction over both would be mostly branches on which kind it is.

// RecurringExpense is a scheduled expense: a rule that creates transactions,
// not a transaction itself. Distinct from a Bucket, which is a monthly budget
// line and creates nothing.
type RecurringExpense struct {
	ID          int64
	HouseholdID int64
	UserID      int64
	Label       string
	Amount      Cents

	// BucketID is the monthly budget line this pays towards, or nil. Essential
	// marks it as a need rather than a want, which is what sizes the emergency
	// fund. Both are carried onto every transaction the schedule creates, so
	// automating an expense does not remove it from the planning it belongs to.
	BucketID  *int64
	Essential bool

	// BucketName is the name of that budget line, or "" when there is none or
	// it has since been archived. Display only.
	BucketName string

	FrequencyN    int
	FrequencyUnit string
	StartDate     string
	NextDueDate   string
	Active        bool
	CreatedAt     string
	UpdatedAt     string
}

// BucketRef is BucketID as a plain number, 0 for none, so a template can
// compare it with a bucket's ID to preselect an option.
func (r RecurringExpense) BucketRef() int64 {
	if r.BucketID == nil {
		return 0
	}
	return *r.BucketID
}

// recurringExpenseSelect is the column list and join shared by every read of
// recurring_expense, in the order scanRecurringExpense expects.
const recurringExpenseSelect = `
	SELECT r.id, r.household_id, r.user_id, r.label, r.amount_cents,
	       r.bucket_id, r.essential, IFNULL(b.name, ''),
	       r.frequency_n, r.frequency_unit, r.start_date, r.next_due_date,
	       r.active, r.created_at, r.updated_at
	FROM recurring_expense r
	LEFT JOIN expense_buckets b ON b.id = r.bucket_id AND b.archived_at IS NULL`

func scanRecurringExpense(row rowScanner) (RecurringExpense, error) {
	var r RecurringExpense
	var amount int64
	var essential, active int
	if err := row.Scan(
		&r.ID, &r.HouseholdID, &r.UserID, &r.Label, &amount,
		&r.BucketID, &essential, &r.BucketName,
		&r.FrequencyN, &r.FrequencyUnit, &r.StartDate, &r.NextDueDate,
		&active, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return RecurringExpense{}, err
	}
	r.Amount = Cents(amount)
	r.Essential = essential == 1
	r.Active = active == 1
	return r, nil
}

// CreateRecurringExpense stores a schedule. The first occurrence is startDate
// itself, which ProcessDueRecurringExpenses will pick up the moment that date
// has arrived -- including immediately, when the user chose today.
func (s *Store) CreateRecurringExpense(
	ctx context.Context,
	sc Scope,
	label string,
	amount Cents,
	bucketID *int64,
	essential bool,
	frequencyN int,
	frequencyUnit string,
	startDate string,
) (int64, error) {
	if amount <= 0 {
		return 0, errors.New("a recurring expense needs an amount greater than zero")
	}
	if ok, msg := ValidFrequency(frequencyN, frequencyUnit); !ok {
		return 0, fmt.Errorf("invalid recurring frequency: %s", msg)
	}
	startDate, err := ParseDate(startDate)
	if err != nil {
		return 0, err
	}

	// A bucket from another household would attach this budget's spending to
	// somebody else's plan, so it is checked rather than trusted.
	if bucketID != nil {
		var ok int
		err := s.db.QueryRowContext(ctx, `
			SELECT 1 FROM expense_buckets
			WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
			*bucketID, sc.HouseholdID).Scan(&ok)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("check bucket: %w", err)
		}
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO recurring_expense (
			household_id, user_id, label, amount_cents,
			bucket_id, essential,
			frequency_n, frequency_unit, start_date, next_due_date
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.HouseholdID, sc.UserID, cleanLabel(label), int64(amount),
		bucketID, boolToInt(essential),
		frequencyN, frequencyUnit, startDate, startDate,
	)
	if err != nil {
		return 0, fmt.Errorf("create recurring expense: %w", err)
	}
	return res.LastInsertId()
}

// ListRecurringExpense returns this budget's schedules, soonest due first.
func (s *Store) ListRecurringExpense(ctx context.Context, sc Scope) ([]RecurringExpense, error) {
	rows, err := s.db.QueryContext(ctx, recurringExpenseSelect+`
		WHERE r.household_id = ?
		ORDER BY r.active DESC, r.next_due_date ASC, r.id ASC`, sc.HouseholdID)
	if err != nil {
		return nil, fmt.Errorf("list recurring expenses: %w", err)
	}
	defer rows.Close()

	out := []RecurringExpense{}
	for rows.Next() {
		r, err := scanRecurringExpense(rows)
		if err != nil {
			return nil, fmt.Errorf("scan recurring expense: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecurringExpenseByID fetches one schedule in this budget.
func (s *Store) RecurringExpenseByID(ctx context.Context, sc Scope, id int64) (RecurringExpense, error) {
	r, err := scanRecurringExpense(s.db.QueryRowContext(ctx, recurringExpenseSelect+`
		WHERE r.id = ? AND r.household_id = ?`, id, sc.HouseholdID))
	if errors.Is(err, sql.ErrNoRows) {
		return RecurringExpense{}, ErrNotFound
	}
	if err != nil {
		return RecurringExpense{}, fmt.Errorf("get recurring expense: %w", err)
	}
	return r, nil
}

// UpdateRecurringExpense changes what an active schedule charges, how often,
// and where it is attributed. It never moves next_due_date: anything already
// owed under the old terms is still owed on the day it fell due, and only the
// occurrences after that reflect the change -- the same rule recurring income
// follows.
func (s *Store) UpdateRecurringExpense(
	ctx context.Context,
	sc Scope,
	id int64,
	label string,
	amount Cents,
	bucketID *int64,
	essential bool,
	frequencyN int,
	frequencyUnit string,
) error {
	label = cleanLabel(label)
	if label == "" {
		return errors.New("a recurring expense needs a name")
	}
	if amount <= 0 {
		return errors.New("a recurring expense needs an amount greater than zero")
	}
	if ok, msg := ValidFrequency(frequencyN, frequencyUnit); !ok {
		return fmt.Errorf("invalid recurring frequency: %s", msg)
	}

	bucket, err := s.resolveBucket(ctx, sc, bucketID)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE recurring_expense
		SET label = ?, amount_cents = ?, bucket_id = ?, essential = ?,
		    frequency_n = ?, frequency_unit = ?, updated_at = datetime('now')
		WHERE id = ? AND household_id = ? AND active = 1`,
		label, int64(amount), bucket, boolToInt(essential),
		frequencyN, frequencyUnit, id, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("update recurring expense: %w", err)
	}
	return requireOneRow(res)
}

// SetRecurringExpenseActive turns a schedule on or off. Turning it off stops
// future occurrences and leaves every transaction it already created alone.
func (s *Store) SetRecurringExpenseActive(ctx context.Context, sc Scope, id int64, active bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE recurring_expense
		SET active = ?, updated_at = datetime('now')
		WHERE id = ? AND household_id = ?`,
		boolToInt(active), id, sc.HouseholdID)
	if err != nil {
		return fmt.Errorf("set recurring expense active: %w", err)
	}
	return requireOneRow(res)
}

// ProcessDueRecurringExpenses creates whatever each schedule owes up to asOf.
//
// Catching up is a loop rather than a single insert because a schedule that has
// not been visited for six weeks owes three fortnightly payments, not one. The
// UNIQUE(schedule, due_date) constraint on the occurrences table is what makes
// running this repeatedly safe: a second run inserts nothing, so refreshing the
// page cannot charge the rent twice.
func (s *Store) ProcessDueRecurringExpenses(ctx context.Context, sc Scope, asOf string) error {
	asOf, err := ParseDate(asOf)
	if err != nil {
		return err
	}

	schedules, err := s.ListRecurringExpense(ctx, sc)
	if err != nil {
		return err
	}

	// See ProcessDueRecurringIncome.
	touched := map[string]bool{}

	for _, r := range schedules {
		if !r.Active {
			continue
		}

		nextDue := r.NextDueDate

		// Bounded: see maxCatchUpOccurrences. Anything still owed after that
		// is generated on the next visit, because next_due_date only ever
		// advances as far as the work actually done.
		for made := 0; nextDue <= asOf && made < maxCatchUpOccurrences; made++ {
			due := nextDue
			err := s.inTx(ctx, func(tx *sql.Tx) error {
				var existing int64
				err := tx.QueryRowContext(ctx, `
					SELECT transaction_id
					FROM recurring_expense_occurrences
					WHERE recurring_expense_id = ? AND due_date = ?`,
					r.ID, due,
				).Scan(&existing)

				// Already generated on a previous run. Nothing to do, and not an
				// error: this is the normal path every time the page is opened.
				if err == nil {
					return nil
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("check recurring expense occurrence: %w", err)
				}

				res, err := tx.ExecContext(ctx, `
					INSERT INTO transactions (
						household_id, user_id, kind, label, amount_cents,
						occurred_on, essential, bucket_id
					)
					VALUES (?, ?, 'expense', ?, ?, ?, ?, ?)`,
					sc.HouseholdID, r.UserID, cleanLabel(r.Label), int64(r.Amount),
					due, boolToInt(r.Essential), r.BucketID,
				)
				if err != nil {
					return fmt.Errorf("create recurring expense transaction: %w", err)
				}

				transactionID, err := res.LastInsertId()
				if err != nil {
					return err
				}

				if _, err = tx.ExecContext(ctx, `
					INSERT INTO recurring_expense_occurrences (
						recurring_expense_id, due_date, transaction_id
					)
					VALUES (?, ?, ?)`,
					r.ID, due, transactionID,
				); err != nil {
					return fmt.Errorf("record recurring expense occurrence: %w", err)
				}

				touched[due[:7]] = true

				return recordAudit(ctx, tx,
					Scope{HouseholdID: sc.HouseholdID, UserID: r.UserID},
					"created", "transaction", transactionID,
					fmt.Sprintf("Recurring expense %s — %q on %s",
						r.Amount.Display(), cleanLabel(r.Label), due),
				)
			})
			if err != nil {
				return err
			}

			nextDue, err = advanceRecurringDate(r.StartDate, due, r.FrequencyN, r.FrequencyUnit)
			if err != nil {
				return err
			}
		}

		if nextDue != r.NextDueDate {
			if _, err := s.db.ExecContext(ctx, `
				UPDATE recurring_expense
				SET next_due_date = ?, updated_at = datetime('now')
				WHERE id = ? AND household_id = ?`,
				nextDue, r.ID, sc.HouseholdID,
			); err != nil {
				return fmt.Errorf("advance recurring expense: %w", err)
			}
		}
	}

	s.reallocateTouched(ctx, sc, touched)
	return nil
}

// reallocateTouched re-pours the funding waterfall for each month a catch-up
// added transactions to. A failure is logged rather than returned: the
// transactions are already committed and next_due_date has moved on, and the
// month is re-poured again by the next edit or by Recalculate.
func (s *Store) reallocateTouched(ctx context.Context, sc Scope, months map[string]bool) {
	for m := range months {
		if err := s.Reallocate(ctx, sc, m); err != nil {
			log.Printf("store: household %d: could not reallocate %s after a recurring catch-up: %v",
				sc.HouseholdID, m, err)
		}
	}
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
