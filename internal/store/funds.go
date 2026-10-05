package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

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

// ErrFutureDated refuses a savings transfer dated after today. Transfers cannot
// be edited or deleted, so one typed as 2199 instead of 2026 would sit in the
// fund for good -- and would stop it ever being closed (see CloseFund).
var ErrFutureDated = errors.New("a savings transfer cannot be dated in the future")

// ErrFutureTransfers refuses to close a fund that holds a transfer dated after
// today: its balance cannot be settled until that date. New transfers can no
// longer be future-dated, so only data from before that rule can reach this.
var ErrFutureTransfers = errors.New("this fund has a transfer dated after today, so it cannot be closed yet")

// notFuture is the date rule every savings transfer shares.
func notFuture(occurredOn string) error {
	if occurredOn > Today() {
		return ErrFutureDated
	}
	return nil
}

// Deposit moves cash into a fund: one row, inserted inside a transaction that first
// re-reads available cash, so a user cannot move in more than they hold and the cash
// side and the fund side cannot disagree.
func (s *Store) Deposit(ctx context.Context, sc Scope, fundID int64, amount Cents, occurredOn string) error {
	if amount <= 0 {
		return fmt.Errorf("deposit must be positive")
	}
	if err := notFuture(occurredOn); err != nil {
		return err
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := fundExists(ctx, tx, sc.HouseholdID, fundID); err != nil {
			return err
		}

		cash, err := lowestBalanceFrom(ctx, tx, `
			SELECT occurred_on, `+cashSignSQL+` FROM transactions WHERE household_id = ?`,
			sc.HouseholdID, occurredOn)
		if err != nil {
			return fmt.Errorf("cash available: %w", err)
		}
		if amount > cash {
			return fmt.Errorf("%w: you have %s available%s", ErrInsufficientCash, cash.Display(), onOrAfter(occurredOn))
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
	if err := notFuture(occurredOn); err != nil {
		return err
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := fundExists(ctx, tx, sc.HouseholdID, fundID); err != nil {
			return err
		}

		balance, err := lowestBalanceFrom(ctx, tx, `
			SELECT occurred_on, CASE kind
				WHEN 'fund_deposit'    THEN  amount_cents
				WHEN 'fund_withdrawal' THEN -amount_cents
				ELSE 0 END
			FROM transactions WHERE fund_id = ?`, fundID, occurredOn)
		if err != nil {
			return fmt.Errorf("fund balance: %w", err)
		}
		if amount > balance {
			return fmt.Errorf("%w: that fund holds %s%s", ErrInsufficientFund, balance.Display(), onOrAfter(occurredOn))
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
		var future int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM transactions WHERE fund_id = ? AND occurred_on > ?`,
			fundID, Today()).Scan(&future); err != nil {
			return fmt.Errorf("check future fund transfers: %w", err)
		}
		if future != 0 {
			return ErrFutureTransfers
		}

		// A close is a withdrawal today.  Use the same date-aware balance rule
		// as Withdraw so deposits dated in the future cannot be returned early.
		balance, err := lowestBalanceFrom(ctx, tx, `
			SELECT occurred_on, CASE kind
				WHEN 'fund_deposit' THEN amount_cents
				WHEN 'fund_withdrawal' THEN -amount_cents
				ELSE 0 END
			FROM transactions WHERE fund_id = ?`, fundID, Today())
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

// lowestBalanceFrom is the most that can be taken out of a running balance on
// date without it going below zero on that date or any day after.
//
// movements is a query yielding (occurred_on, signed amount) rows, with one
// placeholder, bound to owner. Checking only today's balance let a backdated
// deposit pass while leaving the balance negative on the days in between:
// moving $500 into savings "last March" when there was only $50 then.
func lowestBalanceFrom(ctx context.Context, tx *sql.Tx, movements string, owner any, date string) (Cents, error) {
	var lowest int64
	err := tx.QueryRowContext(ctx, `
		WITH m(occurred_on, amount) AS (`+movements+`),
		     daily AS (SELECT occurred_on, SUM(amount) AS amount FROM m GROUP BY occurred_on),
		     running AS (SELECT occurred_on, SUM(amount) OVER (ORDER BY occurred_on) AS balance FROM daily)
		SELECT MIN(
			(SELECT IFNULL(SUM(amount), 0) FROM daily WHERE occurred_on <= ?),
			IFNULL((SELECT MIN(balance) FROM running WHERE occurred_on > ?), 1 << 62))`,
		owner, date, date).Scan(&lowest)
	if err != nil {
		return 0, err
	}
	return Cents(lowest), nil
}

// onOrAfter phrases the date a balance check applied to, for an error message.
func onOrAfter(date string) string {
	if date == "" || date >= Today() {
		return ""
	}
	return " on or after " + date
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
