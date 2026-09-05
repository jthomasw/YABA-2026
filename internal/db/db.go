// Package db opens and configures the SQLite database.
package db

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Open connects to the SQLite file at path with the pragmas this app needs: WAL so a
// reader cannot block a writer, a busy timeout so a contended write waits rather than
// failing instantly, and foreign keys on (SQLite defaults them off).
func Open(path string) (*sql.DB, error) {
	// Pragmas in the DSN are applied to every connection in the pool, which
	// matters because setting them with a one-off Exec only affects whichever
	// pooled connection happened to serve that call.
	dsn := path + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// SQLite allows one writer at a time. A single pooled connection turns lock
	// contention into harmless queueing inside the process instead of SQLITE_BUSY.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return sqlDB, nil
}

// ═════════════════════════════════════════════════════════════════════════════
// migrate.go
// ═════════════════════════════════════════════════════════════════════════════

// Migration is one versioned, run-once change to the schema.
type Migration struct {
	Version int
	Name    string
	Run     func(tx *sql.Tx) error
}

// sqlMigration wraps a list of statements as a Migration.
func sqlMigration(version int, name string, stmts ...string) Migration {
	return Migration{
		Version: version,
		Name:    name,
		Run: func(tx *sql.Tx) error {
			for _, s := range stmts {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("statement failed: %w\n%s", err, s)
				}
			}
			return nil
		},
	}
}

// Migrate applies every migration above the recorded version, each in its own
// transaction.
func Migrate(sqlDB *sql.DB) error {
	if _, err := sqlDB.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := sqlDB.QueryRow(
		`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`,
	).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	applied := 0
	for _, m := range migrations() {
		if m.Version <= current {
			continue
		}

		tx, err := sqlDB.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}

		// PRAGMA foreign_keys is a no-op inside a transaction but defer_foreign_keys is not:
		// migration 1 rebuilds tables in an order that is briefly inconsistent.
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
			tx.Rollback()
			return fmt.Errorf("defer foreign keys: %w", err)
		}

		if err := m.Run(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(version, name) VALUES(?, ?)`,
			m.Version, m.Name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d (%s): %w", m.Version, m.Name, err)
		}

		log.Printf("migration %d applied: %s", m.Version, m.Name)
		applied++
	}

	if applied == 0 {
		log.Printf("schema up to date (version %d)", current)
	}
	return nil
}

func migrations() []Migration {
	return []Migration{
		{Version: 1, Name: "canonical schema and legacy import", Run: migrate001},
		sqlMigration(2, "monthly category budgets", `
			CREATE TABLE budgets (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				category    TEXT    NOT NULL,
				limit_cents INTEGER NOT NULL CHECK (limit_cents > 0),
				created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
				UNIQUE(user_id, category)
			)`,
			`CREATE INDEX idx_budgets_user ON budgets(user_id)`,
		),
		{Version: 3, Name: "email login, expense buckets, line items, receipt queue", Run: migrate003},
		{Version: 4, Name: "shared budgeting: households, members, invitations", Run: migrate004},

		// Migration 5: revocable sessions. A signed cookie can be checked but never
		// cancelled; a row per login can be deleted, so a logout or a password change takes
		// effect on the next request.
		sqlMigration(5, "server-side revocable sessions", `
			CREATE TABLE sessions (
				id           TEXT    PRIMARY KEY,
				user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
				last_seen_at TEXT    NOT NULL DEFAULT (datetime('now')),
				expires_at   TEXT    NOT NULL,
				user_agent   TEXT    NOT NULL DEFAULT ''
			)`,
			// Deleting the account deletes its sessions through the cascade
			// above; this index serves the device list, newest first.
			`CREATE INDEX idx_sessions_user ON sessions(user_id, last_seen_at DESC)`,
			// And this one serves the expiry sweep, which is a range scan.
			`CREATE INDEX idx_sessions_expiry ON sessions(expires_at)`,
		),

		// Five small migrations rather than one. Each has a single reason, so
		// each can be read, reviewed and reasoned about on its own -- and if one
		// of them ever needs undoing, it is the only thing in its transaction.
		sqlMigration(6, "invitations expire", `
			ALTER TABLE household_invites ADD COLUMN expires_at TEXT`,
			// Retroactive by design: existing invitations get the same 24-hour window measured
			// from when they were sent, so an old one shows as expired with a resend button.
			`UPDATE household_invites
			 SET expires_at = datetime(created_at, '+24 hours')
			 WHERE expires_at IS NULL`,
			// The sweep that removes long-dead invitations is a range scan on this column.
			`CREATE INDEX idx_invites_expiry ON household_invites(expires_at)`,
		),

		sqlMigration(7, "password reset tokens", `
			CREATE TABLE password_resets (
				token      TEXT    PRIMARY KEY,
				user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				created_at TEXT    NOT NULL DEFAULT (datetime('now')),
				expires_at TEXT    NOT NULL,
				used_at    TEXT
			)`,
			// Same reasoning as the sessions table: the token IS the credential, so it is a
			// random string and never a predictable integer.
			`CREATE INDEX idx_resets_user ON password_resets(user_id)`,
			`CREATE INDEX idx_resets_expiry ON password_resets(expires_at)`,
		),

		sqlMigration(8, "transaction version for optimistic concurrency", `
			ALTER TABLE transactions ADD COLUMN version INTEGER NOT NULL DEFAULT 1`,
		// Two members opening the same expense both saved, and the second write discarded
		// the first. A version every UPDATE must match turns that into a visible refusal.
		),

		sqlMigration(9, "login attempts survive a restart", `
			CREATE TABLE login_attempts (
				key          TEXT    PRIMARY KEY,
				failures     INTEGER NOT NULL DEFAULT 0,
				window_start TEXT    NOT NULL DEFAULT (datetime('now'))
			)`,
			// The limiter was a map in memory, so restarting the server cleared every lockout --
			// and a process that crashes under load restarts itself.
			`CREATE INDEX idx_attempts_window ON login_attempts(window_start)`,
		),

		sqlMigration(10, "store receipt paths with forward slashes", `
			UPDATE transactions SET receipt_path = REPLACE(receipt_path, '\', '/')
			WHERE receipt_path LIKE '%\%'`,
			`UPDATE receipt_jobs SET path = REPLACE(path, '\', '/')
			 WHERE path LIKE '%\%'`,
			// Paths were written with the running OS's separator, so a database created on
			// Windows held uploads\16\abc.png and could not be read on Linux.
		),

		// Migration 11: one-time form tokens. Redirect-after-POST stops a refresh
		// resubmitting, but not a double click or the back button followed by Save.
		sqlMigration(11, "one-time form tokens", `
			CREATE TABLE form_tokens (
				token      TEXT    PRIMARY KEY,
				user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				purpose    TEXT    NOT NULL DEFAULT '',
				created_at TEXT    NOT NULL DEFAULT (datetime('now'))
			)`,
			`CREATE INDEX idx_form_tokens_age ON form_tokens(created_at)`,
		),

		// Migration 12: the audit log. Every row records who created it, but an edit or a
		// deletion left no trace at all.
		sqlMigration(12, "audit log", `
			CREATE TABLE audit_log (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
				user_id      INTEGER          REFERENCES users(id)      ON DELETE SET NULL,
				action       TEXT    NOT NULL,
				entity       TEXT    NOT NULL,
				entity_id    INTEGER,
				summary      TEXT    NOT NULL DEFAULT '',
				created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
			)`,
			// The only query it serves: this household's history, newest first.
			`CREATE INDEX idx_audit_household ON audit_log(household_id, id DESC)`,
		),

		// Migration 13: what OCR read off a receipt, before anyone has agreed to
		// it. This is a draft, not a transaction -- nothing here has touched the
		// ledger, and nothing will until the user presses Save on a form filled
		// in from these columns.
		sqlMigration(13, "parsed receipt drafts", `
			ALTER TABLE receipt_jobs ADD COLUMN parsed_total_cents INTEGER NOT NULL DEFAULT 0`,
			// A column of its own rather than a field inside the JSON below,
			// because the "waiting for details" list shows the amount against
			// every pending receipt and sorting on it should not mean decoding
			// a document per row.
			`ALTER TABLE receipt_jobs ADD COLUMN parsed_confidence REAL NOT NULL DEFAULT 0`,
			// The rest of the draft -- merchant, category, tax, tip and the line
			// items -- as one JSON document. It is written once by the worker and
			// read once by the form, never queried across, and modelling a
			// throwaway draft as five more columns and a child table would leave
			// rows to garbage-collect for no benefit.
			`ALTER TABLE receipt_jobs ADD COLUMN parsed_json TEXT NOT NULL DEFAULT ''`,
			// The raw OCR text, kept so a user can see what was actually read and
			// so a parser bug is reproducible from the database alone.
			`ALTER TABLE receipt_jobs ADD COLUMN ocr_text TEXT NOT NULL DEFAULT ''`,
		),
	}
}

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

// ── migration 1 ────────────────────────────────────────────────────────────────

// migrate001 installs the canonical schema and imports an old database when present.
func migrate001(tx *sql.Tx) error {
	legacy, err := hasTable(tx, "income")
	if err != nil {
		return err
	}

	// Move colliding legacy tables aside. Nothing is ever dropped: the old rows stay
	// queryable as legacy_* so a bad import can be inspected or redone.
	if legacy {
		for _, name := range []string{
			"users", "funds", "income", "expense", "fund_transactions",
			"emergency_fund", "emergency_goals", "bar",
		} {
			ok, err := hasTable(tx, name)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if _, err := tx.Exec(`ALTER TABLE ` + name + ` RENAME TO legacy_` + name); err != nil {
				return fmt.Errorf("archive %s: %w", name, err)
			}
		}
	}

	for _, stmt := range canonicalSchema {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("create schema: %w\n%s", err, stmt)
		}
	}

	if !legacy {
		return nil
	}
	return importLegacy(tx)
}

var canonicalSchema = []string{
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// A fund holds a name and a target. Its balance is a SUM over
	// transactions, never a stored number.
	`CREATE TABLE funds (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name          TEXT    NOT NULL,
		goal_cents    INTEGER NOT NULL DEFAULT 0 CHECK (goal_cents >= 0),
		target_months INTEGER NOT NULL DEFAULT 0 CHECK (target_months >= 0),
		closed_at     TEXT,
		created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
	)`,

	// One row per movement of money. kind sets the sign: income and fund_withdrawal add to
	// cash, expense and fund_deposit subtract.
	`CREATE TABLE transactions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		kind         TEXT    NOT NULL CHECK (kind IN ('income','expense','fund_deposit','fund_withdrawal')),
		label        TEXT    NOT NULL DEFAULT '',
		amount_cents INTEGER NOT NULL CHECK (amount_cents > 0),
		occurred_on  TEXT    NOT NULL,
		essential    INTEGER CHECK (essential IN (0,1)),
		fund_id      INTEGER REFERENCES funds(id) ON DELETE CASCADE,
		receipt_path TEXT,
		receipt_name TEXT,
		created_at   TEXT    NOT NULL DEFAULT (datetime('now')),

		-- A fund movement must name its fund; a plain income or expense must not.
		CHECK ((kind IN ('fund_deposit','fund_withdrawal')) = (fund_id IS NOT NULL))
	)`,

	`CREATE INDEX idx_tx_user_date    ON transactions(user_id, occurred_on DESC)`,
	`CREATE INDEX idx_tx_user_created ON transactions(user_id, created_at DESC, id DESC)`,
	`CREATE INDEX idx_tx_user_kind    ON transactions(user_id, kind)`,
	`CREATE INDEX idx_tx_fund         ON transactions(fund_id)`,
	`CREATE INDEX idx_funds_user      ON funds(user_id)`,
}

func importLegacy(tx *sql.Tx) error {
	// Users first: everything else resolves user_id against this table.
	if _, err := tx.Exec(`
		INSERT INTO users(id, username, password_hash)
		SELECT id, username, password FROM legacy_users
	`); err != nil {
		return fmt.Errorf("import users: %w", err)
	}

	// Fund ids are preserved so legacy_fund_transactions.fund_id resolves.
	if _, err := tx.Exec(`
		INSERT INTO funds(id, user_id, name, goal_cents)
		SELECT lf.id,
		       u.id,
		       TRIM(IFNULL(lf.name, 'Unnamed fund')),
		       CAST(ROUND(IFNULL(lf.goal, 0) * 100) AS INTEGER)
		FROM legacy_funds lf
		JOIN users u ON u.username = lf.user
	`); err != nil {
		return fmt.Errorf("import funds: %w", err)
	}

	// Income. occurred_on falls back to the date embedded in created_at and
	// then to today, so the NOT NULL constraint always holds.
	if _, err := tx.Exec(`
		INSERT INTO transactions(user_id, kind, label, amount_cents, occurred_on, created_at)
		SELECT u.id,
		       'income',
		       TRIM(IFNULL(li.source, '')),
		       CAST(ROUND(li.amount * 100) AS INTEGER),
		       COALESCE(NULLIF(TRIM(IFNULL(li.date, '')), ''), date('now')),
		       COALESCE(NULLIF(TRIM(IFNULL(li.created_at, '')), ''), '1970-01-01 00:00:00')
		FROM legacy_income li
		JOIN users u ON u.username = li.user
		WHERE li.amount IS NOT NULL AND ROUND(li.amount * 100) > 0
	`); err != nil {
		return fmt.Errorf("import income: %w", err)
	}

	// Expenses. The label lives in different columns depending on how old the row is: the
	// earliest rows put the category in `source`, later ones in `category`.
	labelExpr, err := legacyExpenseLabelExpr(tx)
	if err != nil {
		return err
	}
	essentialExpr, err := legacyEssentialExpr(tx)
	if err != nil {
		return err
	}
	createdExpr, err := legacyCreatedExpr(tx, "legacy_expense")
	if err != nil {
		return err
	}

	// Rows labelled Emergency Fund are skipped: they are the cash side of a deposit that is
	// imported below with a proper fund_id, and importing both double-counts the outflow.
	if _, err := tx.Exec(`
		INSERT INTO transactions(user_id, kind, label, amount_cents, occurred_on, essential, created_at)
		SELECT u.id,
		       'expense',
		       ` + labelExpr + `,
		       CAST(ROUND(le.amount * 100) AS INTEGER),
		       COALESCE(NULLIF(TRIM(IFNULL(le.date, '')), ''), date('now')),
		       ` + essentialExpr + `,
		       ` + createdExpr + `
		FROM legacy_expense le
		JOIN users u ON u.username = le.user
		WHERE le.amount IS NOT NULL
		  AND ROUND(le.amount * 100) > 0
		  AND ` + labelExpr + ` <> 'Emergency Fund'
	`); err != nil {
		return fmt.Errorf("import expenses: %w", err)
	}

	// Fund movements. These carry the fund_id the expense rows never had.
	fundCreatedExpr, err := legacyCreatedExpr(tx, "legacy_fund_transactions")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO transactions(user_id, kind, label, amount_cents, occurred_on, fund_id, created_at)
		SELECT u.id,
		       CASE WHEN LOWER(TRIM(IFNULL(lft.type,''))) = 'withdrawal'
		            THEN 'fund_withdrawal' ELSE 'fund_deposit' END,
		       f.name,
		       CAST(ROUND(lft.amount * 100) AS INTEGER),
		       COALESCE(NULLIF(TRIM(IFNULL(lft.date, '')), ''), date('now')),
		       f.id,
		       ` + fundCreatedExpr + `
		FROM legacy_fund_transactions lft
		JOIN users u ON u.username = lft.user
		JOIN funds f ON f.id = lft.fund_id AND f.user_id = u.id
		WHERE lft.amount IS NOT NULL AND ROUND(lft.amount * 100) > 0
	`); err != nil {
		return fmt.Errorf("import fund transactions: %w", err)
	}

	// legacy_emergency_fund and legacy_emergency_goals are deliberately not imported.

	return reportImport(tx)
}

// reportImport logs a per-user before/after comparison so a bad import is
// visible at startup rather than discovered later by a confused user.
func reportImport(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT u.username,
		       IFNULL(SUM(CASE t.kind WHEN 'income' THEN t.amount_cents
		                              WHEN 'fund_withdrawal' THEN t.amount_cents
		                              ELSE -t.amount_cents END), 0) AS cash_cents,
		       COUNT(t.id)
		FROM users u
		LEFT JOIN transactions t ON t.user_id = u.id
		GROUP BY u.id
		HAVING COUNT(t.id) > 0
		ORDER BY u.username
	`)
	if err != nil {
		return fmt.Errorf("import report: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var username string
		var cash, count int64
		if err := rows.Scan(&username, &cash, &count); err != nil {
			return err
		}
		log.Printf("  imported user=%-14s rows=%-4d cash=%.2f", username, count, float64(cash)/100)
	}
	return rows.Err()
}

// ── introspection helpers ─────────────────────────────────────────────────────

func hasTable(tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look up table %s: %w", name, err)
	}
	return n > 0, nil
}

func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("describe %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// legacyExpenseLabelExpr builds the SQL that recovers an expense's label from
// whichever of the two historical columns is populated.
func legacyExpenseLabelExpr(tx *sql.Tx) (string, error) {
	hasCategory, err := hasColumn(tx, "legacy_expense", "category")
	if err != nil {
		return "", err
	}
	hasSource, err := hasColumn(tx, "legacy_expense", "source")
	if err != nil {
		return "", err
	}

	switch {
	case hasCategory && hasSource:
		return `COALESCE(NULLIF(TRIM(IFNULL(le.category,'')),''), NULLIF(TRIM(IFNULL(le.source,'')),''), 'Uncategorised')`, nil
	case hasCategory:
		return `COALESCE(NULLIF(TRIM(IFNULL(le.category,'')),''), 'Uncategorised')`, nil
	case hasSource:
		return `COALESCE(NULLIF(TRIM(IFNULL(le.source,'')),''), 'Uncategorised')`, nil
	default:
		return `'Uncategorised'`, nil
	}
}

// legacyEssentialExpr maps the old 'Essential'/'Unessential' text to 1/0.
func legacyEssentialExpr(tx *sql.Tx) (string, error) {
	ok, err := hasColumn(tx, "legacy_expense", "essential")
	if err != nil {
		return "", err
	}
	if !ok {
		return `1`, nil
	}
	return `CASE WHEN LOWER(TRIM(IFNULL(le.essential,''))) = 'unessential' THEN 0 ELSE 1 END`, nil
}

// legacyCreatedExpr preserves created_at when the column exists.
func legacyCreatedExpr(tx *sql.Tx, table string) (string, error) {
	ok, err := hasColumn(tx, table, "created_at")
	if err != nil {
		return "", err
	}
	alias := map[string]string{
		"legacy_expense":           "le",
		"legacy_fund_transactions": "lft",
		"legacy_income":            "li",
	}[table]
	if !ok || alias == "" {
		return `'1970-01-01 00:00:00'`, nil
	}
	return `COALESCE(NULLIF(TRIM(IFNULL(` + alias + `.created_at,'')),''), '1970-01-01 00:00:00')`, nil
}

// ═════════════════════════════════════════════════════════════════════════════
// migrate004.go
// ═════════════════════════════════════════════════════════════════════════════

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

// ═════════════════════════════════════════════════════════════════════════════
// backup
// ═════════════════════════════════════════════════════════════════════════════

// In WAL mode the committed state spans the database, its -wal and its -shm.

const (
	// DefaultBackupKeep is how many snapshots survive the retention sweep.
	DefaultBackupKeep = 14

	// DefaultBackupEvery is the interval for the in-process timer.
	DefaultBackupEvery = 24 * time.Hour

	backupPrefix = "yaba-"
	backupExt    = ".db"
	uploadsSuf   = "-uploads.zip"

	// Stamps are UTC so names sort chronologically.
	stampLayout = "20060102-150405Z"
)

// guardedTables are the tables whose emptiness means a broken snapshot rather than a
// small one.
var guardedTables = []string{
	"users", "households", "household_members", "transactions", "funds",
}

// Counts is a row count per table, used to compare one snapshot with the last.
type Counts map[string]int64

// Snapshot describes one completed backup.
type Snapshot struct {
	Path    string        // the .db file written
	Uploads string        // the receipts archive, or "" if there were none
	Bytes   int64         // size of the .db file
	Counts  Counts        // row counts, read back out of the snapshot itself
	Pruned  []string      // files removed by the retention sweep
	Took    time.Duration // wall clock for snapshot plus verification
}

// BackupConfig configures Backup.
type BackupConfig struct {
	// Dir is where snapshots are written. Required.
	Dir string

	// UploadDir is the receipts directory. Optional, but a database-only backup restores
	// rows pointing at files that are not there: receipts live on disk, not in SQLite.
	UploadDir string

	// Keep is how many snapshots to retain. Zero means DefaultBackupKeep.
	Keep int
}

// Backup writes a verified snapshot and sweeps old ones.
func Backup(ctx context.Context, sqlDB *sql.DB, cfg BackupConfig) (Snapshot, error) {
	started := time.Now()

	if strings.TrimSpace(cfg.Dir) == "" {
		return Snapshot{}, errors.New("backup: no directory configured")
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultBackupKeep
	}

	// 0o700: a snapshot is a complete copy of every user's finances, including
	// their bcrypt hashes. It deserves the same protection as the original.
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("backup: create %s: %w", cfg.Dir, err)
	}

	// Read the previous snapshot's counts before writing the new one, so the
	// comparison below has something to compare against.
	previous, prevPath := lastSnapshotCounts(ctx, cfg.Dir)

	path, err := freeSnapshotPath(cfg.Dir, time.Now().UTC())
	if err != nil {
		return Snapshot{}, err
	}

	// VACUUM INTO refuses to overwrite, so a half-written file from a crashed run would
	// block every future backup at this timestamp.
	if _, err := sqlDB.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return Snapshot{}, fmt.Errorf("backup: vacuum into %s: %w", path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("backup: stat %s: %w", path, err)
	}

	counts, err := VerifySnapshot(ctx, path)
	if err != nil {
		// An unusable snapshot is worse than none, because its presence implies
		// a safety that is not there. Delete it and report.
		os.Remove(path)
		return Snapshot{}, fmt.Errorf("backup: %w", err)
	}

	if err := compareCounts(previous, counts, prevPath); err != nil {
		os.Remove(path)
		return Snapshot{}, fmt.Errorf("backup: %w", err)
	}

	snap := Snapshot{Path: path, Bytes: info.Size(), Counts: counts}

	if cfg.UploadDir != "" {
		dest := strings.TrimSuffix(path, backupExt) + uploadsSuf
		n, err := archiveUploads(cfg.UploadDir, dest)
		switch {
		case err != nil:
			// A missing receipt archive does not invalidate the database
			// snapshot, so this is reported rather than fatal.
			log.Printf("backup: could not archive %s: %v", cfg.UploadDir, err)
		case n > 0:
			snap.Uploads = dest
		}
	}

	pruned, err := Prune(cfg.Dir, cfg.Keep)
	if err != nil {
		log.Printf("backup: retention sweep failed: %v", err)
	}
	snap.Pruned = pruned
	snap.Took = time.Since(started)
	return snap, nil
}

// freeSnapshotPath returns a path that does not exist yet: two backups inside one
// second collide, so take the next free suffix rather than fail.
func freeSnapshotPath(dir string, at time.Time) (string, error) {
	base := filepath.Join(dir, backupPrefix+at.Format(stampLayout))
	for i := 0; i < 100; i++ {
		path := base + backupExt
		if i > 0 {
			path = fmt.Sprintf("%s-%d%s", base, i+1, backupExt)
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
	}
	return "", fmt.Errorf("backup: %s is already full of snapshots for this second", dir)
}

// VerifySnapshot proves a snapshot is usable and returns its row counts.
func VerifySnapshot(ctx context.Context, path string) (Counts, error) {
	snap, err := openForVerify(ctx, path)
	if err != nil {
		return nil, err
	}
	defer snap.Close()

	// 1. integrity_check returns one row reading "ok", or many rows describing
	//    the damage.
	rows, err := snap.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("integrity_check: %w", err)
	}
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("snapshot %s failed integrity_check: %s",
			filepath.Base(path), strings.Join(problems[:min(len(problems), 3)], "; "))
	}

	// 2. Dangling references.
	fk, err := snap.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	broken := 0
	for fk.Next() {
		broken++
	}
	fkErr := fk.Err()
	fk.Close()
	if fkErr != nil {
		return nil, fmt.Errorf("foreign_key_check on %s: %w", filepath.Base(path), fkErr)
	}
	if broken > 0 {
		return nil, fmt.Errorf("snapshot %s has %d dangling foreign key(s)",
			filepath.Base(path), broken)
	}

	// 3. The schema must be one this build understands, and the application's
	//    own invariants must hold.
	var version int
	if err := snap.QueryRowContext(ctx,
		`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return nil, fmt.Errorf("snapshot %s has no schema_migrations: %w",
			filepath.Base(path), err)
	}
	if version == 0 {
		return nil, fmt.Errorf("snapshot %s records no applied migrations", filepath.Base(path))
	}

	var unowned, unhoused int
	if err := snap.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM households h
		         WHERE NOT EXISTS (SELECT 1 FROM household_members m
		                            WHERE m.household_id = h.id AND m.role = 'owner')),
		       (SELECT COUNT(*) FROM users u
		         WHERE NOT EXISTS (SELECT 1 FROM household_members m
		                            WHERE m.user_id = u.id))`,
	).Scan(&unowned, &unhoused); err != nil {
		return nil, fmt.Errorf("snapshot %s: invariant query failed: %w",
			filepath.Base(path), err)
	}
	if unowned > 0 {
		return nil, fmt.Errorf("snapshot %s has %d household(s) with no owner",
			filepath.Base(path), unowned)
	}
	if unhoused > 0 {
		return nil, fmt.Errorf("snapshot %s has %d user(s) with no household",
			filepath.Base(path), unhoused)
	}

	// 4. Counts, for the caller to compare against the previous snapshot.
	counts := Counts{}
	for _, t := range guardedTables {
		var n int64
		if err := snap.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM "`+t+`"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("snapshot %s: count %s: %w", filepath.Base(path), t, err)
		}
		counts[t] = n
	}
	return counts, nil
}

// openForVerify opens a snapshot read-only. query_only stops the verification
// modifying what it verifies, but a driver that refuses the pragma falls back rather
// than blocking startup on every pending migration.
func openForVerify(ctx context.Context, path string) (*sql.DB, error) {
	attempts := []string{
		"?_pragma=query_only(true)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
		"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
	}
	var lastErr error
	for i, params := range attempts {
		snap, err := sql.Open("sqlite", path+params)
		if err == nil {
			snap.SetMaxOpenConns(1)
			if err = snap.PingContext(ctx); err == nil {
				if i > 0 {
					log.Printf("backup: query_only is unavailable, verifying %s without it",
						filepath.Base(path))
				}
				return snap, nil
			}
			snap.Close()
		}
		lastErr = err
	}
	return nil, fmt.Errorf("snapshot %s is not a readable database: %w",
		filepath.Base(path), lastErr)
}

// compareCounts rejects the shape of a truncated backup: a guarded table that had rows
// and now has none.
func compareCounts(prev, now Counts, prevPath string) error {
	if prev == nil {
		return nil
	}
	for _, t := range guardedTables {
		was, is := prev[t], now[t]
		if was > 0 && is == 0 {
			return fmt.Errorf("snapshot looks truncated: %s held %d row(s) in %s and holds none now",
				t, was, filepath.Base(prevPath))
		}
		if is < was {
			log.Printf("backup: %s shrank from %d to %d since %s "+
				"(expected after a deliberate deletion, suspicious otherwise)",
				t, was, is, filepath.Base(prevPath))
		}
	}
	return nil
}

// lastSnapshotCounts reads the counts out of the newest snapshot itself rather than a
// sidecar, because a record of what a backup holds is only as trustworthy as its
// agreement with the backup.
func lastSnapshotCounts(ctx context.Context, dir string) (Counts, string) {
	found, err := Snapshots(dir)
	if err != nil || len(found) == 0 {
		return nil, ""
	}
	newest := found[len(found)-1]
	counts, err := VerifySnapshot(ctx, newest)
	if err != nil {
		// The previous snapshot being bad is not a reason to refuse to take a new one.
		log.Printf("backup: previous snapshot %s does not verify: %v",
			filepath.Base(newest), err)
		return nil, ""
	}
	return counts, newest
}

// Snapshots lists snapshot paths in the directory, oldest first.
func Snapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var found []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, backupExt) {
			continue
		}
		found = append(found, filepath.Join(dir, n))
	}
	// Names carry a UTC timestamp, so lexical order is chronological order.
	sort.Strings(found)
	return found, nil
}

// Prune keeps the newest keep snapshots and the receipt archive belonging to each.
func Prune(dir string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	found, err := Snapshots(dir)
	if err != nil {
		return nil, err
	}
	if len(found) <= keep {
		return nil, nil
	}

	var removed []string
	for _, path := range found[:len(found)-keep] {
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("prune %s: %w", path, err)
		}
		removed = append(removed, path)

		// The receipts archive is part of the same snapshot, so it goes at the same time.
		archive := strings.TrimSuffix(path, backupExt) + uploadsSuf
		if err := os.Remove(archive); err == nil {
			removed = append(removed, archive)
		} else if !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("prune %s: %w", archive, err)
		}
	}
	return removed, nil
}

// archiveUploads zips the receipt directory alongside the snapshot.
func archiveUploads(uploadDir, dest string) (int, error) {
	info, err := os.Stat(uploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", uploadDir)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(out)

	stored := 0
	walkErr := filepath.WalkDir(uploadDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(uploadDir, path)
		if err != nil {
			return err
		}
		// Always forward slashes: a zip written on Windows with backslashes in
		// its entry names does not extract correctly anywhere else.
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		stored++
		return nil
	})

	// The zip writer flushes its central directory on Close, so it must close before the
	// file beneath it.
	zipErr := zw.Close()
	fileErr := out.Close()

	for _, err := range []error{walkErr, zipErr, fileErr} {
		if err != nil {
			os.Remove(dest)
			return 0, err
		}
	}
	if stored == 0 {
		// An empty archive is noise in the backup directory.
		os.Remove(dest)
	}
	return stored, nil
}

// Restore puts a snapshot back at dbPath, verifying it first so a corrupt backup
// cannot destroy a working database on its way to being discovered.
func Restore(ctx context.Context, snapshotPath, dbPath string, force bool) error {
	if _, err := VerifySnapshot(ctx, snapshotPath); err != nil {
		return fmt.Errorf("restore refused: %w", err)
	}

	if _, err := os.Stat(dbPath); err == nil && !force {
		return fmt.Errorf("%s already exists; pass -force to overwrite it", dbPath)
	}

	if err := copyFileTo(snapshotPath, dbPath); err != nil {
		return err
	}

	// Delete the old sidecars: a -wal left from the database being replaced belongs to a
	// different file, and SQLite would try to recover it into the restored one.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restore: remove stale %s%s: %w", dbPath, suffix, err)
		}
	}
	return nil
}

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to a temporary name in the destination directory and rename, so an interrupted
	// copy cannot leave half a database where the real one was.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Pending reports how many migrations would run against this database, which is what
// decides whether startup takes a backup first.
func Pending(sqlDB *sql.DB) (int, error) {
	var current int
	err := sqlDB.QueryRow(`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`).Scan(&current)
	if err != nil {
		// No schema_migrations table means an empty or pre-versioning database,
		// so everything is pending.
		current = 0
	}
	pending := 0
	for _, m := range migrations() {
		if m.Version > current {
			pending++
		}
	}
	return pending, nil
}

// Version reports the highest applied migration version, and 0 for a database that has
// never been migrated.
func Version(sqlDB *sql.DB) int {
	var v int
	if err := sqlDB.QueryRow(
		`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0
	}
	return v
}

// BackupLoop takes a snapshot on a timer until ctx is cancelled, inside the server
// process, so backups do not depend on an external scheduler.
func BackupLoop(ctx context.Context, sqlDB *sql.DB, cfg BackupConfig, every time.Duration) {
	if every <= 0 {
		every = DefaultBackupEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap, err := Backup(ctx, sqlDB, cfg)
			if err != nil {
				// Loud, and every time. A backup that quietly stopped working two months ago is
				// worse than none at all, because it removed the worry without removing the risk.
				log.Printf("BACKUP FAILED: %v", err)
				continue
			}
			log.Printf("backup: %s (%s) verified in %s%s",
				filepath.Base(snap.Path), humanBytes(snap.Bytes),
				snap.Took.Round(time.Millisecond), prunedNote(snap.Pruned))
		}
	}
}

func prunedNote(pruned []string) string {
	if len(pruned) == 0 {
		return ""
	}
	return fmt.Sprintf(", pruned %d old file(s)", len(pruned))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// DefaultBackupDir is outside the project folder: a cloud-synced directory would upload
// every snapshot forever, and a backup beside its original dies with it.
func DefaultBackupDir() string {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "YABA", "backups")
	}
	return "backups"
}
