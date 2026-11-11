package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

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

	// Payee, Place and Note are the Who, Where and Why from the form, copied
	// onto every expense the schedule creates. Empty when not given.
	Payee string
	Place string
	Note  string

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

// MonthlyEquivalent is what the schedule costs in an average month: a weekly
// $50 is about $216.67, every two months $300 is $150. Used to size things
// measured per month, such as the emergency fund's essentials.
func (r RecurringExpense) MonthlyEquivalent() Cents {
	return monthlyEquivalent(r.Amount, r.FrequencyN, r.FrequencyUnit)
}

// monthlyEquivalent converts an amount paid every n days, weeks or months to
// an average month, using 365.25/12 days per month.
func monthlyEquivalent(amount Cents, n int, unit string) Cents {
	if n <= 0 || amount <= 0 {
		return 0
	}
	var perMonth float64
	switch unit {
	case "day":
		perMonth = float64(amount) * (365.25 / 12) / float64(n)
	case "week":
		perMonth = float64(amount) * (365.25 / 12 / 7) / float64(n)
	case "month":
		perMonth = float64(amount) / float64(n)
	default:
		return 0
	}
	return Cents(int64(perMonth + 0.5))
}

// recurringExpenseSelect is the column list and join shared by every read of
// recurring_expense, in the order scanRecurringExpense expects.
const recurringExpenseSelect = `
	SELECT r.id, r.household_id, r.user_id, r.label, r.amount_cents,
	       r.bucket_id, r.essential, IFNULL(b.name, ''),
	       r.frequency_n, r.frequency_unit, r.start_date, r.next_due_date,
	       r.active, r.created_at, r.updated_at,
	       r.payee, r.place, r.note
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
		&r.Payee, &r.Place, &r.Note,
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
	return s.CreateRecurringExpenseWith(ctx, sc, label, amount, bucketID, essential,
		frequencyN, frequencyUnit, startDate, ExpenseDetails{})
}

// ExpenseDetails are the optional Who, Where and Why of an expense.
type ExpenseDetails struct {
	Payee, Place, Note string
}

// CreateRecurringExpenseWith is CreateRecurringExpense with the optional Who,
// Where and Why, which every expense the schedule creates then carries.
func (s *Store) CreateRecurringExpenseWith(
	ctx context.Context,
	sc Scope,
	label string,
	amount Cents,
	bucketID *int64,
	essential bool,
	frequencyN int,
	frequencyUnit string,
	startDate string,
	d ExpenseDetails,
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
			frequency_n, frequency_unit, start_date, next_due_date,
			payee, place, note
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.HouseholdID, sc.UserID, cleanLabel(label), int64(amount),
		bucketID, boolToInt(essential),
		frequencyN, frequencyUnit, startDate, startDate,
		cleanLabel(d.Payee), cleanLabel(d.Place), cleanNote(d.Note),
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
						occurred_on, essential, bucket_id, payee, place, note
					)
					VALUES (?, ?, 'expense', ?, ?, ?, ?, ?, ?, ?, ?)`,
					sc.HouseholdID, r.UserID, cleanLabel(r.Label), int64(r.Amount),
					due, boolToInt(r.Essential), r.BucketID, r.Payee, r.Place, r.Note,
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
	// Back-posted months also change what the current month expects to need
	// (see ReallocateMonthOf), so it is re-poured as well.
	if len(months) > 0 {
		months[Today()[:7]] = true
	}
	for m := range months {
		if err := s.Reallocate(ctx, sc, m); err != nil {
			log.Printf("store: household %d: could not reallocate %s after a recurring catch-up: %v",
				sc.HouseholdID, m, err)
		}
	}
}
