package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
