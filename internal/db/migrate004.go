package db

import (
	"database/sql"
	"fmt"
	"log"
)

// Migration 4: shared budgeting. A household owns money.
func migrate004(tx *sql.Tx) error {
	stmts := []string{
		// personal_for marks a household as one person's private space, and is the join key
		// the backfill below uses to find each user's household.
		`CREATE TABLE households (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT    NOT NULL,
			personal_for INTEGER REFERENCES users(id) ON DELETE CASCADE,
			created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		// Partial, not plain unique: SQLite treats every NULL as distinct, so UNIQUE alone
		// would permit only a single shared household in the entire database.
		`CREATE UNIQUE INDEX idx_households_personal
			ON households(personal_for) WHERE personal_for IS NOT NULL`,

		// The role lives on the membership, not the user: the same person owns their own
		// household and may be a viewer of somebody else's.
		`CREATE TABLE household_members (
			household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
			user_id      INTEGER NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
			role         TEXT    NOT NULL CHECK (role IN ('owner','editor','viewer')),
			joined_at    TEXT    NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (household_id, user_id)
		)`,
		// The composite primary key already indexes household_id first; this
		// covers the other direction, "which households is this user in?",
		// which runs on every authenticated request.
		`CREATE INDEX idx_members_user ON household_members(user_id)`,

		// Keyed by email, so somebody can be invited before they have an account.
		`CREATE TABLE household_invites (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
			email        TEXT    NOT NULL,
			role         TEXT    NOT NULL CHECK (role IN ('editor','viewer')),
			invited_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
			status       TEXT    NOT NULL DEFAULT 'pending'
			                     CHECK (status IN ('pending','accepted','declined','revoked')),
			created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
			responded_at TEXT
		)`,
		// One *open* invitation per address per household.
		`CREATE UNIQUE INDEX idx_invites_open
			ON household_invites(household_id, email) WHERE status = 'pending'`,
		// The lookup on every page load: "are there invitations for me?"
		`CREATE INDEX idx_invites_email ON household_invites(email, status)`,

		// SQLite allows REFERENCES on ADD COLUMN only with a NULL default.
		`ALTER TABLE users ADD COLUMN active_household_id
			INTEGER REFERENCES households(id) ON DELETE SET NULL`,

		// display_name was backfilled from username in migration 3 and is NOT NULL DEFAULT
		// empty, so the COALESCE only has to cover the empty string.
		`INSERT INTO households(name, personal_for, created_at)
			SELECT COALESCE(NULLIF(TRIM(u.display_name), ''),
			                NULLIF(TRIM(u.username), ''),
			                'Household') || '''s budget',
			       u.id,
			       u.created_at
			FROM users u`,

		`INSERT INTO household_members(household_id, user_id, role, joined_at)
			SELECT h.id, h.personal_for, 'owner', h.created_at
			FROM households h WHERE h.personal_for IS NOT NULL`,

		`UPDATE users SET active_household_id =
			(SELECT h.id FROM households h WHERE h.personal_for = users.id)`,

		// The five household-scoped tables move to household ownership.
		`ALTER TABLE transactions ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,
		`ALTER TABLE funds ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,
		`ALTER TABLE expense_buckets ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,
		`ALTER TABLE allocations ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,
		`ALTER TABLE budgets ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,

		// receipt_jobs records the household at upload time, which is intent rather than
		// ownership: the worker runs later, and the expense must land in the budget the user
		// was looking at when they chose the file, not whichever they have switched to since.
		`ALTER TABLE receipt_jobs ADD COLUMN household_id
			INTEGER REFERENCES households(id) ON DELETE CASCADE`,

		// Backfill. Every existing row was created by one user working alone,
		// so it belongs in that user's personal household.
		`UPDATE transactions    SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = transactions.user_id)`,
		`UPDATE funds           SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = funds.user_id)`,
		`UPDATE expense_buckets SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = expense_buckets.user_id)`,
		`UPDATE allocations     SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = allocations.user_id)`,
		`UPDATE budgets         SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = budgets.user_id)`,
		`UPDATE receipt_jobs    SET household_id = (SELECT h.id FROM households h WHERE h.personal_for = receipt_jobs.user_id)`,

		// Every dashboard query now filters on household_id, so these replace the composite
		// indexes from migrations 1 and 3. The user_id indexes stay for attribution queries.
		`CREATE INDEX idx_tx_hh_date      ON transactions(household_id, occurred_on DESC)`,
		`CREATE INDEX idx_tx_hh_created   ON transactions(household_id, created_at DESC, id DESC)`,
		`CREATE INDEX idx_tx_hh_kind      ON transactions(household_id, kind)`,
		`CREATE INDEX idx_funds_hh        ON funds(household_id)`,
		`CREATE INDEX idx_buckets_hh_prio ON expense_buckets(household_id, priority ASC, id ASC)`,
		`CREATE INDEX idx_alloc_hh_month  ON allocations(household_id, month)`,

		// Strictly stronger than the inline UNIQUE(user_id, category) this supersedes -- see
		// the header comment.
		`CREATE UNIQUE INDEX idx_budgets_hh_cat ON budgets(household_id, category)`,

		// One emergency fund per household rather than per member.
		`DROP INDEX IF EXISTS idx_funds_one_emergency`,
		`CREATE UNIQUE INDEX idx_funds_one_emergency
			ON funds(household_id) WHERE is_emergency = 1 AND closed_at IS NULL`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\n%s", err, s)
		}
	}

	if err := assertBackfilled(tx); err != nil {
		return err
	}
	return reportHouseholds(tx)
}

// assertBackfilled refuses to commit if any row was left without a household.
func assertBackfilled(tx *sql.Tx) error {
	for _, table := range []string{
		"transactions", "funds", "expense_buckets", "allocations", "budgets",
		"receipt_jobs",
	} {
		var orphans int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` WHERE household_id IS NULL`,
		).Scan(&orphans); err != nil {
			return fmt.Errorf("check %s backfill: %w", table, err)
		}
		if orphans > 0 {
			return fmt.Errorf(
				"%s: %d row(s) could not be assigned a household -- "+
					"this means a row references a user_id with no personal household, "+
					"so the migration has been rolled back rather than hide them",
				table, orphans)
		}
	}

	// The mirror of the above: a user with no household cannot log in, because
	// there would be nothing to show them.
	var homeless int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM users WHERE active_household_id IS NULL`,
	).Scan(&homeless); err != nil {
		return fmt.Errorf("check user households: %w", err)
	}
	if homeless > 0 {
		return fmt.Errorf("%d user(s) have no household", homeless)
	}

	return nil
}

// reportHouseholds logs what the migration produced, in the same spirit as
// reportImport: a bad backfill should be visible at startup rather than
// discovered later by a user whose dashboard has gone blank.
func reportHouseholds(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT h.name,
		       (SELECT COUNT(*) FROM household_members m WHERE m.household_id = h.id),
		       (SELECT COUNT(*) FROM transactions t      WHERE t.household_id = h.id),
		       (SELECT COUNT(*) FROM funds f             WHERE f.household_id = h.id)
		FROM households h
		ORDER BY h.id`)
	if err != nil {
		return fmt.Errorf("household report: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var members, txs, funds int
		if err := rows.Scan(&name, &members, &txs, &funds); err != nil {
			return err
		}
		log.Printf("  household %-28q members=%d transactions=%-4d funds=%d",
			name, members, txs, funds)
	}
	return rows.Err()
}
