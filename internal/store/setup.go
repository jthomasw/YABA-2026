package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type SetupExpense struct {
	Name          string
	Amount        Cents
	OccurredOn    string
	Recurring     bool
	FrequencyN    int
	FrequencyUnit string
	Essential     bool
}

type SetupIncome struct {
	Source        string
	Amount        Cents
	OccurredOn    string
	Recurring     bool
	FrequencyN    int
	FrequencyUnit string
}

// InitializeBudget records the starting balance and initial entries together,
// so a failed setup cannot leave only part of it saved.
func (s *Store) InitializeBudget(ctx context.Context, sc Scope, startingBalance Cents, expenses []SetupExpense, incomes []SetupIncome) error {
	if startingBalance < 0 {
		return errors.New("setup amounts cannot be negative")
	}

	today := Today()
	for i := range expenses {
		expense := &expenses[i]
		expense.Name = cleanLabel(expense.Name)
		if expense.Name == "" {
			return errors.New("an initial expense needs a name")
		}
		if expense.Amount <= 0 {
			return errors.New("an initial expense needs an amount greater than zero")
		}
		occurredOn, err := ParseDate(expense.OccurredOn)
		if err != nil {
			return fmt.Errorf("invalid initial expense date: %w", err)
		}
		expense.OccurredOn = occurredOn
		if expense.Recurring {
			if ok, msg := ValidFrequency(expense.FrequencyN, expense.FrequencyUnit); !ok {
				return fmt.Errorf("invalid recurring expense frequency: %s", msg)
			}
		}
	}
	for i := range incomes {
		income := &incomes[i]
		income.Source = cleanLabel(income.Source)
		if income.Source == "" {
			return errors.New("an initial income needs a source")
		}
		if income.Amount <= 0 {
			return errors.New("an initial income needs an amount greater than zero")
		}
		occurredOn, err := ParseDate(income.OccurredOn)
		if err != nil {
			return fmt.Errorf("invalid initial income date: %w", err)
		}
		income.OccurredOn = occurredOn
		if income.Recurring {
			if ok, msg := ValidFrequency(income.FrequencyN, income.FrequencyUnit); !ok {
				return fmt.Errorf("invalid recurring income frequency: %s", msg)
			}
		}
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if startingBalance > 0 {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO transactions
					(household_id, user_id, kind, label, amount_cents, occurred_on)
				VALUES (?, ?, 'income', 'Starting balance', ?, ?)`,
				sc.HouseholdID, sc.UserID, int64(startingBalance), today)
			if err != nil {
				return fmt.Errorf("record starting balance: %w", err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := recordAudit(ctx, tx, sc, "created", "transaction", id,
				fmt.Sprintf("Starting balance %s on %s", startingBalance.Display(), today)); err != nil {
				return err
			}
		}

		for _, expense := range expenses {
			if expense.Recurring {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO recurring_expense (
						household_id, user_id, label, amount_cents, essential,
						frequency_n, frequency_unit, start_date, next_due_date
					)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					sc.HouseholdID, sc.UserID, expense.Name, int64(expense.Amount),
					boolToInt(expense.Essential), expense.FrequencyN, expense.FrequencyUnit,
					expense.OccurredOn, expense.OccurredOn); err != nil {
					return fmt.Errorf("record recurring expense %q: %w", expense.Name, err)
				}
				continue
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO transactions
					(household_id, user_id, kind, label, amount_cents, occurred_on, essential)
				VALUES (?, ?, 'expense', ?, ?, ?, ?)`,
				sc.HouseholdID, sc.UserID, expense.Name, int64(expense.Amount),
				expense.OccurredOn, boolToInt(expense.Essential))
			if err != nil {
				return fmt.Errorf("record initial expense %q: %w", expense.Name, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := recordAudit(ctx, tx, sc, "created", "transaction", id,
				fmt.Sprintf("Expense %s — %q on %s", expense.Amount.Display(), expense.Name, expense.OccurredOn)); err != nil {
				return err
			}
		}

		for _, income := range incomes {
			if income.Recurring {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO recurring_income (
						household_id, user_id, source, amount_cents,
						frequency_n, frequency_unit, start_date, next_due_date
					)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					sc.HouseholdID, sc.UserID, income.Source, int64(income.Amount),
					income.FrequencyN, income.FrequencyUnit, income.OccurredOn, income.OccurredOn); err != nil {
					return fmt.Errorf("record recurring income %q: %w", income.Source, err)
				}
				continue
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO transactions
					(household_id, user_id, kind, label, amount_cents, occurred_on)
				VALUES (?, ?, 'income', ?, ?, ?)`,
				sc.HouseholdID, sc.UserID, income.Source, int64(income.Amount), income.OccurredOn)
			if err != nil {
				return fmt.Errorf("record initial income %q: %w", income.Source, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := recordAudit(ctx, tx, sc, "created", "transaction", id,
				fmt.Sprintf("Income %s — %q on %s", income.Amount.Display(), income.Source, income.OccurredOn)); err != nil {
				return err
			}
		}

		return nil
	})
}
