package store

import (
	"context"
	"database/sql"
	"fmt"
)

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

	// Only allocations to buckets that still exist count.
	//
	// Required is summed over active buckets, so Allocated has to be too, or the
	// two halves of the same sentence disagree. Archiving a bucket re-pours only
	// the CURRENT month, so its allocations survive in every other month; without
	// this join, viewing one of those months showed money committed to a budget
	// line that appears nowhere on the page -- $1,050.00 allocated against
	// $1,000.00 required, and $50.00 of the user's income invisible.
	if err := s.db.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(a.amount_cents), 0)
		FROM allocations a
		JOIN expense_buckets b ON b.id = a.bucket_id
		WHERE a.household_id = ? AND a.month = ? AND b.archived_at IS NULL`,
		sc.HouseholdID, month).Scan(&sum.Allocated); err != nil {
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
