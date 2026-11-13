package db

import (
	"database/sql"
	"fmt"
	"log"
)

// ── migration 3 ────────────────────────────────────────────────────────────────

// migrate003 adds email accounts, priority-ordered expense buckets with income
// allocation, multi-line transactions, the receipt queue and notifications.
func migrate003(tx *sql.Tx) error {
	stmts := []string{
		// Login is by email address. Existing accounts have a username instead, so email is
		// backfilled from it and legacy users keep signing in with the string they know.
		`ALTER TABLE users ADD COLUMN email TEXT`,
		`ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`UPDATE users SET email = username WHERE email IS NULL`,
		`UPDATE users SET display_name = username WHERE display_name = ''`,

		// Case-SENSITIVE deliberately. A NOCASE index is the better rule for new accounts,
		// but the old UNIQUE(username) was case-sensitive and this database already holds two
		// accounts differing only in case, so a NOCASE index could not be created at all.
		`CREATE UNIQUE INDEX idx_users_email ON users(email)`,

		// Recurring monthly expenses, ranked by which must be paid first.
		`CREATE TABLE expense_buckets (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name        TEXT    NOT NULL,
			priority    INTEGER NOT NULL DEFAULT 0,
			cost_kind   TEXT    NOT NULL DEFAULT 'fixed' CHECK (cost_kind IN ('fixed','variable')),
			fixed_cents INTEGER NOT NULL DEFAULT 0 CHECK (fixed_cents >= 0),
			essential   INTEGER NOT NULL DEFAULT 0 CHECK (essential IN (0,1)),
			archived_at TEXT,
			created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,
		// priority is the sort key for the waterfall, so it is the index.
		`CREATE INDEX idx_buckets_user_priority ON expense_buckets(user_id, priority ASC, id ASC)`,

		// A transaction may be attributed to a bucket, which is how a variable bucket learns
		// what it costs. SQLite permits ADD COLUMN with REFERENCES only when the default is NULL.
		`ALTER TABLE transactions ADD COLUMN bucket_id INTEGER REFERENCES expense_buckets(id) ON DELETE SET NULL`,
		`CREATE INDEX idx_tx_bucket ON transactions(bucket_id)`,

		// ── the 5W expense form ─────────────────────────────────────────────
		// Who / What / When / Where / Why / Amount. "What" is the existing
		// label column and "When" is occurred_on; the other three are new.
		`ALTER TABLE transactions ADD COLUMN payee TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE transactions ADD COLUMN place TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE transactions ADD COLUMN note  TEXT NOT NULL DEFAULT ''`,

		// One row per (income, bucket, month) slice. The FK to the income cascades, so
		// deleting an income unwinds every allocation it funded with no reconciliation job.
		`CREATE TABLE allocations (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			income_id    INTEGER NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
			bucket_id    INTEGER NOT NULL REFERENCES expense_buckets(id) ON DELETE CASCADE,
			month        TEXT    NOT NULL,
			amount_cents INTEGER NOT NULL CHECK (amount_cents > 0),
			created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX idx_alloc_user_month ON allocations(user_id, month)`,
		`CREATE INDEX idx_alloc_bucket     ON allocations(bucket_id, month)`,
		`CREATE INDEX idx_alloc_income     ON allocations(income_id)`,

		// Optional per-item breakdown. A transaction with no line items is its own single
		// implicit item, so nothing existing needs backfilling.
		`CREATE TABLE line_items (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			transaction_id INTEGER NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
			description    TEXT    NOT NULL DEFAULT '',
			category       TEXT    NOT NULL DEFAULT '',
			amount_cents   INTEGER NOT NULL CHECK (amount_cents > 0),
			position       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_items_tx ON line_items(transaction_id, position ASC, id ASC)`,

		// A partial unique index allows exactly one emergency fund per user while leaving the
		// zeros unconstrained; a plain UNIQUE would permit only one ordinary fund each.
		`ALTER TABLE funds ADD COLUMN is_emergency INTEGER NOT NULL DEFAULT 0 CHECK (is_emergency IN (0,1))`,
		`CREATE UNIQUE INDEX idx_funds_one_emergency
			ON funds(user_id) WHERE is_emergency = 1 AND closed_at IS NULL`,

		// ── asynchronous receipt processing ─────────────────────────────────
		// "Picture gets put into a queue server side for processing and the
		// user can go about the rest of their business."
		`CREATE TABLE receipt_jobs (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			path           TEXT    NOT NULL,
			original_name  TEXT    NOT NULL DEFAULT '',
			status         TEXT    NOT NULL DEFAULT 'queued'
			                       CHECK (status IN ('queued','processing','done','failed')),
			error          TEXT    NOT NULL DEFAULT '',
			attempts       INTEGER NOT NULL DEFAULT 0,
			transaction_id INTEGER REFERENCES transactions(id) ON DELETE SET NULL,
			created_at     TEXT    NOT NULL DEFAULT (datetime('now')),
			started_at     TEXT,
			finished_at    TEXT
		)`,
		// The worker's claim query filters on status and orders by id, so this
		// index is the one it rides.
		`CREATE INDEX idx_jobs_status ON receipt_jobs(status, id ASC)`,
		`CREATE INDEX idx_jobs_user   ON receipt_jobs(user_id, created_at DESC)`,

		// Notifications live in a table rather than the session, so one raised while the user
		// was away is still waiting whenever they return.
		`CREATE TABLE notifications (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			kind       TEXT    NOT NULL DEFAULT 'info'
			                   CHECK (kind IN ('info','success','error')),
			text       TEXT    NOT NULL,
			link       TEXT    NOT NULL DEFAULT '',
			created_at TEXT    NOT NULL DEFAULT (datetime('now')),
			seen_at    TEXT
		)`,
		`CREATE INDEX idx_notifications_unseen
			ON notifications(user_id, id ASC) WHERE seen_at IS NULL`,
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\n%s", err, s)
		}
	}

	return reportEmailCollisions(tx)
}

// reportEmailCollisions warns about accounts whose addresses differ only in case.
func reportEmailCollisions(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT LOWER(TRIM(email)) AS key,
		       COUNT(*),
		       GROUP_CONCAT(email || ' (id ' || id || ')', ', ')
		FROM users
		WHERE email IS NOT NULL AND TRIM(email) <> ''
		GROUP BY key
		HAVING COUNT(*) > 1
		ORDER BY key`)
	if err != nil {
		return fmt.Errorf("check email collisions: %w", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var key, detail string
		var n int
		if err := rows.Scan(&key, &n, &detail); err != nil {
			return err
		}
		if !found {
			log.Printf("NOTE: some accounts differ only by capitalisation.")
			log.Printf("      They are kept separate, and each signs in with its exact spelling.")
			found = true
		}
		log.Printf("      %d accounts for %q: %s", n, key, detail)
	}
	return rows.Err()
}
