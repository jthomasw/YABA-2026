// Package db opens and configures the SQLite database.
package db

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// OpenReadOnly opens a second, read-only handle on the database at path, for
// taking backups.
//
// The server's own handle is capped at one connection, so a backup run through
// it -- VACUUM INTO reads the whole database -- held up every request until it
// finished. In WAL mode a separate reader does not block the writer, nor the
// writer it, so snapshots no longer freeze the site.
func OpenReadOnly(path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q read-only: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %q read-only: %w", path, err)
	}
	return sqlDB, nil
}

// fileURI turns a filesystem path into a SQLite "file:" URI, escaping the
// characters a URI would otherwise read as syntax.
func fileURI(path string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23", " ", "%20")
	return "file:" + r.Replace(filepath.ToSlash(path))
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

		sqlMigration(14, "recurring income schedules", `
			CREATE TABLE recurring_income (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				household_id  INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
				user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				source        TEXT    NOT NULL,
				amount_cents  INTEGER NOT NULL CHECK (amount_cents > 0),

				frequency_n   INTEGER NOT NULL CHECK (frequency_n > 0),
				frequency_unit TEXT NOT NULL CHECK (
					frequency_unit IN ('day', 'week', 'month')
				),

				start_date    TEXT NOT NULL,
				next_due_date TEXT NOT NULL,

				active        INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
				created_at    TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
			)`,

			`CREATE INDEX idx_recurring_income_household
		ON recurring_income(household_id, active, next_due_date)`,

			`CREATE INDEX idx_recurring_income_user
		ON recurring_income(user_id)`,
		),

		sqlMigration(15, "recurring income occurrence tracking", `
			CREATE TABLE recurring_income_occurrences (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			recurring_income_id  INTEGER NOT NULL
				REFERENCES recurring_income(id) ON DELETE CASCADE,
			due_date             TEXT NOT NULL,
			transaction_id       INTEGER NOT NULL
				REFERENCES transactions(id) ON DELETE CASCADE,
			created_at           TEXT NOT NULL DEFAULT (datetime('now')),

			UNIQUE(recurring_income_id, due_date)
			)`,

			`CREATE INDEX idx_recurring_income_occurrences_transaction
		 ON recurring_income_occurrences(transaction_id)`,
		),

		// The expense counterpart of 14 and 15, in one migration because the two
		// tables are meaningless apart -- a schedule with nowhere to record what
		// it already generated would double-charge on the next restart.
		//
		// Two columns the income version has no use for. bucket_id keeps an
		// automated expense inside the monthly plan: rent logged by a schedule
		// still counts against its bucket, and essential still sizes the
		// emergency fund. Automating an expense should not quietly remove it
		// from the budgeting it belongs to.
		//
		// bucket_id is SET NULL rather than CASCADE: retiring a budget line is
		// not a reason to stop paying the rent, so the schedule survives and
		// simply stops being attributed.
		sqlMigration(16, "recurring expense schedules", `
			CREATE TABLE recurring_expense (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				household_id  INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
				user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				label         TEXT    NOT NULL,
				amount_cents  INTEGER NOT NULL CHECK (amount_cents > 0),

				bucket_id     INTEGER REFERENCES expense_buckets(id) ON DELETE SET NULL,
				essential     INTEGER NOT NULL DEFAULT 1 CHECK (essential IN (0, 1)),

				frequency_n   INTEGER NOT NULL CHECK (frequency_n > 0),
				frequency_unit TEXT NOT NULL CHECK (
					frequency_unit IN ('day', 'week', 'month')
				),

				start_date    TEXT NOT NULL,
				next_due_date TEXT NOT NULL,

				active        INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
				created_at    TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
			)`,

			`CREATE INDEX idx_recurring_expense_household
		ON recurring_expense(household_id, active, next_due_date)`,

			`CREATE INDEX idx_recurring_expense_user
		ON recurring_expense(user_id)`,

			// UNIQUE(schedule, due_date) is what makes catching up idempotent:
			// the same due date can be inserted only once however many times the
			// processor runs, so a page refresh cannot create the rent twice.
			`CREATE TABLE recurring_expense_occurrences (
				id                    INTEGER PRIMARY KEY AUTOINCREMENT,
				recurring_expense_id  INTEGER NOT NULL
					REFERENCES recurring_expense(id) ON DELETE CASCADE,
				due_date              TEXT NOT NULL,
				transaction_id        INTEGER NOT NULL
					REFERENCES transactions(id) ON DELETE CASCADE,
				created_at            TEXT NOT NULL DEFAULT (datetime('now')),

				UNIQUE(recurring_expense_id, due_date)
			)`,

			`CREATE INDEX idx_recurring_expense_occurrences_transaction
		 ON recurring_expense_occurrences(transaction_id)`,
		),

		// Migration 17 finishes a job migration 4 left half done, and closes the
		// gap it left behind.
		//
		// Budgets began as a per-USER thing (migration 2), with the ownership
		// written into the table as an inline UNIQUE(user_id, category).
		// Migration 4 moved ownership to the household and added a unique index
		// on (household_id, category) -- but an inline UNIQUE builds an implicit
		// index that no DROP INDEX can reach, so the user-scoped rule stayed
		// live underneath. The visible symptom: somebody in two households could
		// budget "Food" in the first and then got a raw SQLite constraint error
		// from the second, forever, because SetBudget's upsert names only the
		// household index as its conflict target and so never sees the other one
		// coming. Dropping it needs the table rebuilt.
		//
		// The second defect is in the replacement index. Spending is matched to
		// a budget case-insensitively (LOWER(TRIM(...))), but the index that is
		// supposed to stop duplicate categories compares bytes -- so "Food" and
		// "food" are two rows to the index and one category to the query, and a
		// single $100.00 expense was counted in full against BOTH, producing two
		// over-budget warnings for one piece of spending. COLLATE NOCASE makes
		// the constraint agree with the query it protects.
		sqlMigration(17, "budgets: drop the stale per-user uniqueness, match categories case-insensitively", `
			CREATE TABLE budgets_new (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id      INTEGER NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
				household_id INTEGER          REFERENCES households(id) ON DELETE CASCADE,
				category     TEXT    NOT NULL,
				limit_cents  INTEGER NOT NULL CHECK (limit_cents > 0),
				created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
			)`,

			// Case-duplicate rows cannot both survive the new index. Keep the
			// newest of each set -- it is the one the user set most recently, so
			// it is the limit they last meant -- and drop the older spellings.
			`INSERT INTO budgets_new (id, user_id, household_id, category, limit_cents, created_at)
			 SELECT b.id, b.user_id, b.household_id, b.category, b.limit_cents, b.created_at
			 FROM budgets b
			 WHERE b.id = (
				SELECT b2.id FROM budgets b2
				WHERE b2.household_id IS b.household_id
				  AND LOWER(TRIM(b2.category)) = LOWER(TRIM(b.category))
				ORDER BY b2.id DESC
				LIMIT 1
			 )`,

			`DROP TABLE budgets`,
			`ALTER TABLE budgets_new RENAME TO budgets`,

			`CREATE INDEX idx_budgets_user ON budgets(user_id)`,
			`CREATE UNIQUE INDEX idx_budgets_hh_cat
				ON budgets(household_id, category COLLATE NOCASE)`,
		),

		// Migration 18 gives a failed receipt somewhere to wait. A failure used
		// to put the job straight back to 'queued', where the worker's drain
		// loop claimed it again in the same breath -- production logs showed all
		// three attempts inside one second, so a 30-second blip at Google cost
		// the user their receipt. The claim query now skips a job until this
		// time has passed. NULL means "due now", which is every existing row and
		// every fresh upload.
		sqlMigration(18, "receipt jobs wait between retries", `
			ALTER TABLE receipt_jobs ADD COLUMN next_attempt_at TEXT`,
		),

		// Monthly schedules used to be stepped with Go's AddDate, which turns
		// Jan 31 + 1 month into Mar 3: a schedule anchored on the 29th-31st was
		// pushed into the first days of the month after any shorter one, and
		// stayed there. The stepping is fixed in the store; this repairs, once,
		// the next_due_date such schedules were left with. See migrate019.
		{Version: 19, Name: "repair monthly schedules drifted off a 29th-31st anchor", Run: migrate019},

		// Migration 20 lets a line item be negative: a discount or coupon on a
		// receipt is a real line, and refusing it meant a scanned receipt with a
		// coupon could not be saved with its breakdown. Zero is still refused.
		// SQLite cannot alter a CHECK constraint, so the table is rebuilt.
		sqlMigration(20, "line items may be negative (discounts)",
			`CREATE TABLE line_items_new (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				transaction_id INTEGER NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
				description    TEXT    NOT NULL DEFAULT '',
				category       TEXT    NOT NULL DEFAULT '',
				amount_cents   INTEGER NOT NULL CHECK (amount_cents <> 0),
				position       INTEGER NOT NULL DEFAULT 0
			)`,
			`INSERT INTO line_items_new (id, transaction_id, description, category, amount_cents, position)
				SELECT id, transaction_id, description, category, amount_cents, position FROM line_items`,
			`DROP TABLE line_items`,
			`ALTER TABLE line_items_new RENAME TO line_items`,
			`CREATE INDEX idx_items_tx ON line_items(transaction_id, position ASC, id ASC)`,
		),

		// Migration 21 stops storing bearer tokens. See migrate021.
		{Version: 21, Name: "store session and reset tokens hashed", Run: migrate021},

		// A snapshot made before shared households existed is still a valid snapshot
		// to take before migrating it.  The verifier selects its invariant checks
		// from this recorded version.
		{Version: 22, Name: "snapshot verification supports pre-household schemas", Run: func(*sql.Tx) error { return nil }},
		{Version: 23, Name: "form tokens can be restored after a refused write", Run: migrate023},
	}
}

func migrate023(tx *sql.Tx) error {
	rows, err := tx.Query(`PRAGMA table_info(form_tokens)`)
	if err != nil {
		return fmt.Errorf("inspect form tokens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("read form token column: %w", err)
		}
		if name == "used_at" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE form_tokens ADD COLUMN used_at TEXT`); err != nil {
		return fmt.Errorf("add form token use marker: %w", err)
	}
	return nil
}

// ── migration 21 ───────────────────────────────────────────────────────────────

// migrate021 replaces every stored session id and password-reset token with its
// SHA-256, matching store.TokenHash. The browser keeps the raw token, so every
// existing login keeps working and every reset link already emailed still
// works: the lookup now hashes what it is given before comparing.
//
// The hashing is repeated here rather than imported from store, because db must
// not depend on the package that depends on it.
func migrate021(tx *sql.Tx) error {
	for _, t := range []struct{ table, column string }{
		{"sessions", "id"},
		{"password_resets", "token"},
	} {
		rows, err := tx.Query(`SELECT ` + t.column + ` FROM ` + t.table)
		if err != nil {
			return fmt.Errorf("read %s: %w", t.table, err)
		}
		var tokens []string
		for rows.Next() {
			var tok string
			if err := rows.Scan(&tok); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s: %w", t.table, err)
			}
			tokens = append(tokens, tok)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, tok := range tokens {
			sum := sha256.Sum256([]byte(tok))
			if _, err := tx.Exec(`UPDATE `+t.table+` SET `+t.column+` = ? WHERE `+t.column+` = ?`,
				hex.EncodeToString(sum[:]), tok); err != nil {
				return fmt.Errorf("hash %s: %w", t.table, err)
			}
		}
	}
	return nil
}

// ── migration 19 ───────────────────────────────────────────────────────────────

// migrate019 moves a drifted monthly next_due_date back to its anchor's day.
//
// Only the pattern the old arithmetic produces is touched: a monthly schedule
// whose start_date falls on the 29th-31st, whose next_due_date falls on the
// 1st-3rd of a later month. The date moves to the anchor's day (clamped to the
// month's length) in the SAME month, so it only ever moves later and never out
// of its month -- Jun 3 becomes Jun 30 -- and that month still gets exactly
// one charge. February, which the drift skipped, is not backfilled: nothing
// records which month a drifted payment was meant for, and a guessed backdated
// charge is worse than a missing one the user can add.
//
// A row whose drifted date has ALREADY been posted (a catch-up interrupted
// after posting but before saving next_due_date) is left alone. The catch-up
// then finds that occurrence, skips it, and steps on from it with the fixed
// arithmetic -- whereas moving it would have charged that month a second time
// under a new date.
//
// This runs once rather than on every catch-up, so a schedule later switched
// from weeks to months, whose date is off its anchor's day for a legitimate
// reason, is never "repaired".
func migrate019(tx *sql.Tx) error {
	for _, t := range []struct{ schedules, occurrences, fk string }{
		{"recurring_income", "recurring_income_occurrences", "recurring_income_id"},
		{"recurring_expense", "recurring_expense_occurrences", "recurring_expense_id"},
	} {
		// Table names come from the fixed list above, never from input.
		stmt := `
			UPDATE ` + t.schedules + `
			SET next_due_date = date(next_due_date, 'start of month',
				'+' || (MIN(
					CAST(strftime('%d', start_date) AS INTEGER),
					CAST(strftime('%d', date(next_due_date, 'start of month', '+1 month', '-1 day')) AS INTEGER)
				) - 1) || ' days')
			WHERE frequency_unit = 'month'
			  AND CAST(strftime('%d', start_date) AS INTEGER) > 28
			  AND CAST(strftime('%d', next_due_date) AS INTEGER) <= 3
			  AND next_due_date > start_date
			  AND NOT EXISTS (
				SELECT 1 FROM ` + t.occurrences + ` o
				WHERE o.` + t.fk + ` = ` + t.schedules + `.id
				  AND o.due_date = ` + t.schedules + `.next_due_date)`
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("repair drifted %s: %w", t.schedules, err)
		}
	}
	return nil
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

	// DefaultConfirmEmptyAfter is how long a guarded table has to stay empty, across
	// at least two backup attempts, before Backup believes it is the database's real
	// state rather than a broken snapshot. See acceptEmptied.
	DefaultConfirmEmptyAfter = time.Hour

	// AcceptEmptyEnv, set to a true value, makes every backup accept a guarded table
	// that has gone empty. It exists so a server whose backups are being refused can be
	// unstuck without a rebuild; unset it again afterwards, or the guard stays off.
	AcceptEmptyEnv = "YABA_BACKUP_ACCEPT_EMPTY"

	backupPrefix = "yaba-"
	backupExt    = ".db"
	uploadsSuf   = "-uploads.zip"

	// preEmptyPrefix marks the last snapshot that still held rows in a table which has
	// since gone empty. Snapshots does not list it, so retention never removes it.
	preEmptyPrefix = "pre-empty-"

	// emptiedMarker records, in the backup directory, when the current emptied state
	// was first seen, so a later attempt can tell "still empty" from "newly empty".
	emptiedMarker = ".yaba-emptied"

	// Stamps are UTC, and a second snapshot within one second gets a "-2", "-3"...
	// suffix. Ordering is by the parsed (stamp, suffix) pair, never by name: '-' sorts
	// before '.', so "…Z-2.db" would sort ahead of the "…Z.db" it follows.
	stampLayout = "20060102-150405Z"
)

// Startup and retry timing for BackupLoop. Variables rather than constants so the
// tests do not have to wait a minute.
var (
	// backupStartDelay is the shortest wait before the first scheduled backup. Long
	// enough that a server crash-looping under systemd does not snapshot on every
	// restart, short enough that a server restarted daily still gets backed up.
	backupStartDelay = time.Minute

	// backupRetryAfter caps the wait after a failed backup, so one transient failure
	// does not cost a whole interval of protection.
	backupRetryAfter = time.Hour
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

	// AcceptEmpty accepts a snapshot in which a guarded table has gone from rows to
	// none, for an operator who knows the deletion was deliberate (`yaba backup
	// -accept-empty`). AcceptEmptyEnv does the same for a process that cannot be
	// given a flag.
	AcceptEmpty bool

	// ConfirmEmptyAfter is how long an emptied table must persist before it is
	// accepted without an operator. Zero means DefaultConfirmEmptyAfter.
	ConfirmEmptyAfter time.Duration
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
		var emptied *EmptiedError
		if !errors.As(err, &emptied) {
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: %w", err)
		}
		accepted, why := acceptEmptied(cfg, emptied, time.Now())
		if !accepted {
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: %w; %s", err, why)
		}
		log.Printf("backup: WARNING: accepting a snapshot in which %s went empty: %s",
			strings.Join(emptied.Tables, ", "), why)
		if kept, err := keepPreEmpty(prevPath); err != nil {
			log.Printf("backup: could not preserve %s outside retention: %v",
				filepath.Base(prevPath), err)
		} else {
			log.Printf("backup: kept %s outside retention, as the last snapshot holding those rows",
				filepath.Base(kept))
		}
	}
	// Whether the counts were normal or an emptied state has just been accepted,
	// nothing is pending confirmation any more.
	clearEmptiedMarker(cfg.Dir)

	snap := Snapshot{Path: path, Bytes: info.Size(), Counts: counts}

	if cfg.UploadDir != "" {
		dest := strings.TrimSuffix(path, backupExt) + uploadsSuf
		n, err := archiveUploads(cfg.UploadDir, dest)
		switch {
		case err != nil:
			// The database refers to these files.  Reporting a snapshot as complete
			// while pruning the last complete pair makes recovery less safe than no
			// backup at all.
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: archive %s: %w", cfg.UploadDir, err)
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
// second collide, so take a suffix rather than fail.
//
// The suffix is one past the highest already used for this second, not the first
// free one. Otherwise, once retention had removed "…Z.db" and kept "…Z-2.db", the
// next backup in that second would reuse "…Z.db", which sorts as the older of the
// two, and the following prune would delete the snapshot just taken.
func freeSnapshotPath(dir string, at time.Time) (string, error) {
	base := filepath.Join(dir, backupPrefix+at.Format(stampLayout))
	stamp := at.Truncate(time.Second)

	highest := 0
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if t, seq, ok := parseSnapshotName(e.Name()); ok && t.Equal(stamp) && seq > highest {
				highest = seq
			}
		}
	}
	for seq := highest + 1; seq <= highest+100; seq++ {
		path := base + backupExt
		if seq > 1 {
			path = fmt.Sprintf("%s-%d%s", base, seq, backupExt)
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

	if version < 4 {
		return snapshotCounts(ctx, snap)
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

func snapshotCounts(ctx context.Context, snap *sql.DB) (Counts, error) {
	counts := Counts{}
	for _, table := range guardedTables {
		var exists int
		if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			continue
		}
		var n int64
		if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = n
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

// EmptiedError is the shape of a truncated backup: one or more guarded tables that
// held rows in the previous snapshot and hold none in this one.
type EmptiedError struct {
	Tables   []string // guarded tables that went from rows to none, in guardedTables order
	Was      Counts   // their counts in the previous snapshot
	Previous string   // the previous snapshot's path
}

func (e *EmptiedError) Error() string {
	parts := make([]string, len(e.Tables))
	for i, t := range e.Tables {
		parts[i] = fmt.Sprintf("%s held %d row(s)", t, e.Was[t])
	}
	return fmt.Sprintf("snapshot looks truncated: %s in %s and holds none now",
		strings.Join(parts, ", "), filepath.Base(e.Previous))
}

// key identifies one emptied state, so a marker left by an earlier attempt is only
// taken as confirmation of the same thing.
func (e *EmptiedError) key() string {
	return filepath.Base(e.Previous) + " " + strings.Join(e.Tables, ",")
}

// compareCounts rejects the shape of a truncated backup: a guarded table that had rows
// and now has none. Shrinking without reaching zero is only logged, because deliberate
// deletions (`yaba reset -keep`, a user closing their account) do exactly that.
func compareCounts(prev, now Counts, prevPath string) error {
	if prev == nil {
		return nil
	}
	var emptied *EmptiedError
	for _, t := range guardedTables {
		was, is := prev[t], now[t]
		if was > 0 && is == 0 {
			if emptied == nil {
				emptied = &EmptiedError{Was: Counts{}, Previous: prevPath}
			}
			emptied.Tables = append(emptied.Tables, t)
			emptied.Was[t] = was
			continue
		}
		if is < was {
			log.Printf("backup: %s shrank from %d to %d since %s "+
				"(expected after a deliberate deletion, suspicious otherwise)",
				t, was, is, filepath.Base(prevPath))
		}
	}
	if emptied != nil {
		return emptied
	}
	return nil
}

// acceptEmptied decides whether an emptied guarded table is the database's real state.
//
// Refusing forever is not an option: the comparison is against the newest surviving
// snapshot, and a refused snapshot is deleted, so once the only household is
// legitimately deleted every later backup would be refused too and the server would
// silently go without backups for good. So the emptied state is accepted when either
//
//   - an operator says so (cfg.AcceptEmpty, or AcceptEmptyEnv), or
//   - the same tables have been seen empty, against the same previous snapshot, on
//     an earlier attempt at least ConfirmEmptyAfter ago. A snapshot that is broken
//     by accident does not keep coming out broken in exactly the same way across
//     attempts an hour apart; a database that really is empty does.
//
// Either way the snapshot that still held the rows is then preserved outside
// retention (keepPreEmpty), so accepting cannot rotate the last good copy away.
func acceptEmptied(cfg BackupConfig, e *EmptiedError, now time.Time) (bool, string) {
	if cfg.AcceptEmpty {
		return true, "accepted by the operator (-accept-empty)"
	}
	if envTrue(os.Getenv(AcceptEmptyEnv)) {
		return true, AcceptEmptyEnv + " is set (unset it again, or this check stays off)"
	}

	wait := cfg.ConfirmEmptyAfter
	if wait <= 0 {
		wait = DefaultConfirmEmptyAfter
	}
	marker := filepath.Join(cfg.Dir, emptiedMarker)
	hint := fmt.Sprintf("if that deletion was deliberate, run `yaba backup -accept-empty` "+
		"or set %s=1 once", AcceptEmptyEnv)

	if key, first, ok := readEmptiedMarker(marker); ok && key == e.key() {
		if seen := now.Sub(first); seen >= wait {
			return true, fmt.Sprintf("the same table(s) have been empty on every attempt since %s (%s ago), "+
				"so this is the database's real state", first.UTC().Format(time.RFC3339),
				seen.Round(time.Second))
		}
		return false, fmt.Sprintf("first seen empty at %s; it will be accepted automatically "+
			"if it is still the case after %s, or %s",
			first.UTC().Format(time.RFC3339), first.Add(wait).UTC().Format(time.RFC3339), hint)
	}

	content := e.key() + "\n" + now.UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(marker, []byte(content), 0o600); err != nil {
		log.Printf("backup: could not record %s: %v", marker, err)
	}
	return false, fmt.Sprintf("it will be accepted automatically if it is still the case after %s, or %s",
		now.Add(wait).UTC().Format(time.RFC3339), hint)
}

func readEmptiedMarker(path string) (key string, first time.Time, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		return "", time.Time{}, false
	}
	first, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(lines[1]))
	if err != nil {
		return "", time.Time{}, false
	}
	return strings.TrimSpace(lines[0]), first, true
}

func clearEmptiedMarker(dir string) {
	if err := os.Remove(filepath.Join(dir, emptiedMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("backup: could not remove %s: %v", emptiedMarker, err)
	}
}

// keepPreEmpty preserves the snapshot that still held the rows, and its receipts, under
// a name Snapshots does not list. A hard link costs no space; a copy is the fallback
// for filesystems without links.
func keepPreEmpty(prevPath string) (string, error) {
	dir, base := filepath.Split(prevPath)
	kept := filepath.Join(dir, preEmptyPrefix+base)
	pairs := [][2]string{{prevPath, kept}}
	archive := strings.TrimSuffix(prevPath, backupExt) + uploadsSuf
	if _, err := os.Stat(archive); err == nil {
		pairs = append(pairs, [2]string{archive, filepath.Join(dir, preEmptyPrefix+filepath.Base(archive))})
	}
	for _, p := range pairs {
		if _, err := os.Stat(p[1]); err == nil {
			continue // already preserved by an earlier acceptance
		}
		if err := os.Link(p[0], p[1]); err != nil {
			if err := copyFileTo(p[0], p[1]); err != nil {
				return "", err
			}
		}
	}
	return kept, nil
}

// envTrue reads a boolean the way the server's own envBool does (strconv.ParseBool).
// Anything unparseable is false: this switches a safety check off, so a typo must not.
func envTrue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("WARNING: %s=%q is not a boolean; treating it as false", AcceptEmptyEnv, v)
		return false
	}
	return b
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

// parseSnapshotName reads the timestamp and same-second sequence number out of a
// snapshot's file name: "yaba-20260801-030000Z.db" is (that instant, 1) and
// "yaba-20260801-030000Z-2.db" is (that instant, 2). ok is false for anything
// freeSnapshotPath would not have written.
func parseSnapshotName(name string) (at time.Time, seq int, ok bool) {
	if !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupExt) {
		return time.Time{}, 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix), backupExt)
	if len(rest) < len(stampLayout) {
		return time.Time{}, 0, false
	}
	at, err := time.Parse(stampLayout, rest[:len(stampLayout)])
	if err != nil {
		return time.Time{}, 0, false
	}
	suffix := rest[len(stampLayout):]
	if suffix == "" {
		return at, 1, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(suffix, "-"))
	if !strings.HasPrefix(suffix, "-") || err != nil || n < 2 {
		return time.Time{}, 0, false
	}
	return at, n, true
}

// Snapshots lists snapshot paths in the directory, oldest first.
//
// Only names freeSnapshotPath writes are listed. Prune deletes from the front of this
// list, so a file that merely happens to start with "yaba-" and end in ".db" must not
// be on it.
func Snapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	type named struct {
		path string
		at   time.Time
		seq  int
	}
	var found []named
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		at, seq, ok := parseSnapshotName(e.Name())
		if !ok {
			continue
		}
		found = append(found, named{filepath.Join(dir, e.Name()), at, seq})
	}
	// By (timestamp, sequence), not by name: '-' sorts before '.', so lexically
	// "…Z-2.db" comes before the "…Z.db" it was written after, and prune, the
	// truncation baseline and "restore the newest" would all pick the wrong file.
	sort.Slice(found, func(i, j int) bool {
		if !found[i].at.Equal(found[j].at) {
			return found[i].at.Before(found[j].at)
		}
		return found[i].seq < found[j].seq
	})
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths, nil
}

// newestSnapshotTime is when the newest snapshot in dir was taken, read from its name.
func newestSnapshotTime(dir string) (time.Time, bool) {
	found, err := Snapshots(dir)
	if err != nil || len(found) == 0 {
		return time.Time{}, false
	}
	at, _, ok := parseSnapshotName(filepath.Base(found[len(found)-1]))
	return at, ok
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
//
// Receipts change far less often than the database, and every backup used to
// zip all of them again, so a site keeping fourteen snapshots held fourteen
// copies of every receipt. Each archive now records a fingerprint of what it
// holds (in the zip comment); when the newest existing archive already holds
// exactly these files, the new one is a hard link to it -- the same bytes under
// a second name, costing no space, and still removed independently by Prune.
// Restore is unchanged: every snapshot still has its own archive.
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

	fingerprint, files, err := uploadsFingerprint(uploadDir)
	if err != nil || files == 0 {
		return 0, err
	}
	if prev := newestArchiveWith(filepath.Dir(dest), fingerprint); prev != "" {
		if err := os.Link(prev, dest); err == nil {
			return files, nil
		}
		// A filesystem without hard links: write a fresh archive instead.
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(out)
	if err := zw.SetComment(fingerprint); err != nil {
		zw.Close()
		out.Close()
		os.Remove(dest)
		return 0, err
	}

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

// uploadsFingerprint summarises the receipt directory as a hash of every file's
// relative path, size and modification time, and counts the files. Receipts are
// written once and never edited, so this changes exactly when one is added or
// removed.
func uploadsFingerprint(dir string) (string, int, error) {
	h := sha256.New()
	files := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		files++
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return "yaba-uploads-v1 " + hex.EncodeToString(h.Sum(nil)), files, nil
}

// newestArchiveWith returns the newest receipt archive in dir whose recorded
// fingerprint is want, or "" when the newest archive differs (or none exists).
// Only the newest is checked: it is the one the last backup wrote.
func newestArchiveWith(dir, want string) string {
	found, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*"+uploadsSuf))
	if err != nil || len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	newest := found[len(found)-1]
	zr, err := zip.OpenReader(newest)
	if err != nil {
		return ""
	}
	defer zr.Close()
	if zr.Comment != want {
		return ""
	}
	return newest
}

// sidecars are the files that, in WAL mode, hold part of a database's committed
// state. They belong with the main file wherever it goes.
var sidecars = []string{"-wal", "-shm"}

// Restore puts a snapshot back at dbPath, verifying it first so a corrupt backup
// cannot destroy a working database on its way to being discovered.
//
// A database already at dbPath is replaced only with force, and even then it is never
// deleted: it is checkpointed and moved aside, together with any -wal and -shm, as
// dbPath.before-restore-<stamp>[-wal|-shm]. The paths it was moved to are returned.
// Because the sidecars keep their suffix relative to the new name, opening the
// set-aside file with SQLite still sees every commit it held.
//
// The caller must hold the database's lock (AcquireLock) for the duration. Moving a
// database out from under a running server would leave the server writing to a file
// that is no longer the database, and every write it accepted afterwards would vanish.
func Restore(ctx context.Context, snapshotPath, dbPath string, force bool) ([]string, error) {
	if _, err := VerifySnapshot(ctx, snapshotPath); err != nil {
		return nil, fmt.Errorf("restore refused: %w", err)
	}

	existing, statErr := os.Stat(dbPath)
	if statErr == nil && !force {
		return nil, fmt.Errorf("%s already exists; pass -force to overwrite it", dbPath)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("restore: %w", statErr)
	}
	if statErr == nil && existing.IsDir() {
		return nil, fmt.Errorf("restore: %s is a directory", dbPath)
	}

	// Anything at dbPath, including sidecars orphaned by a crash, is set aside rather
	// than deleted. An orphaned -wal left in place would be replayed into the restored
	// file, which it does not belong to.
	aside, err := setAside(ctx, dbPath, "before-restore")
	if err != nil {
		return nil, fmt.Errorf("restore: %w; nothing was replaced", err)
	}

	if err := copyFileTo(snapshotPath, dbPath); err != nil {
		// Put the original back, so a failed restore leaves things as they were.
		if rbErr := moveBack(aside, dbPath); rbErr != nil {
			return aside, fmt.Errorf("restore: copy %s: %w (and moving the original back failed: %v; "+
				"it is at %s)", snapshotPath, err, rbErr, strings.Join(aside, ", "))
		}
		return nil, fmt.Errorf("restore: copy %s: %w; the original is back in place", snapshotPath, err)
	}

	// A restore is typically run as root (sudo) on behalf of a service account. A
	// replacement owned by root would leave the server unable to open its own database,
	// so the new file takes the owner of the one it replaced.
	if existing != nil {
		if err := matchOwner(dbPath, existing); err != nil {
			log.Printf("restore: could not give %s the owner of the database it replaced: %v", dbPath, err)
		}
	}
	return aside, nil
}

// setAside moves the database at dbPath, and any sidecars, to dbPath.<label>-<stamp>,
// first folding the WAL into the main file where it can. It returns the new paths, the
// main file first, or nil if there was nothing to move.
func setAside(ctx context.Context, dbPath, label string) ([]string, error) {
	present := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	mainExists := present(dbPath)
	var parts []string // suffixes to move: "" for the main file
	if mainExists {
		parts = append(parts, "")
	}
	for _, s := range sidecars {
		if present(dbPath + s) {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if !mainExists {
		log.Printf("restore: %s is missing but has sidecars; moving them aside", dbPath)
	}

	base, err := freeAsidePath(dbPath, label, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	// Fold uncheckpointed commits into the main file, so the set-aside copy is complete
	// on its own. If that is impossible -- the file being replaced is often the damaged
	// one -- the sidecars are moved with it instead, which loses nothing either.
	//
	// SQLite is not trusted with the only copy of the WAL while it tries: on a file it
	// cannot read as a database it unlinks the -wal and -shm when the connection
	// closes. So a raw copy goes to the set-aside name first, and is replaced by the
	// original below if the original survives, or discarded once a checkpoint has
	// provably folded every frame into the main file.
	walCopy := ""
	if mainExists && isSQLiteFile(dbPath) {
		if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() > 0 {
			if err := copyFileTo(dbPath+"-wal", base+"-wal"); err != nil {
				return nil, fmt.Errorf("preserve %s-wal before checkpointing: %w", dbPath, err)
			}
			walCopy = base + "-wal"
		}
		if err := checkpoint(ctx, dbPath); err != nil {
			log.Printf("restore: could not checkpoint %s (%v); moving its -wal and -shm aside with it",
				dbPath, err)
		} else if walCopy != "" {
			os.Remove(walCopy)
			walCopy = ""
		}
		// A clean checkpoint and close normally removes the sidecars, so look again.
		parts = parts[:1]
		for _, s := range sidecars {
			if present(dbPath + s) {
				parts = append(parts, s)
			}
		}
	}

	// Main file first: if a later rename fails, the ones already done are undone, so the
	// set is never split between two names.
	var moved []string
	for _, s := range parts {
		if err := os.Rename(dbPath+s, base+s); err != nil {
			if rbErr := moveBack(moved, dbPath); rbErr != nil {
				return moved, fmt.Errorf("move %s%s aside: %w (and undoing the earlier moves failed: %v)",
					dbPath, s, err, rbErr)
			}
			if walCopy != "" {
				return nil, fmt.Errorf("move %s%s aside: %w (a copy of its WAL is at %s)",
					dbPath, s, err, walCopy)
			}
			return nil, fmt.Errorf("move %s%s aside: %w", dbPath, s, err)
		}
		moved = append(moved, base+s)
		if base+s == walCopy {
			walCopy = "" // the original replaced the copy
		}
	}
	if walCopy != "" {
		// The checkpoint failed and SQLite removed the original: the copy is the WAL now.
		moved = append(moved, walCopy)
	}
	return moved, nil
}

// isSQLiteFile reports whether path starts with the SQLite header. Anything else is
// moved aside byte for byte without SQLite ever opening it.
func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, 16)
	if _, err := io.ReadFull(f, header); err != nil {
		return false
	}
	return string(header) == "SQLite format 3\x00"
}

// freeAsidePath returns dbPath.<label>-<stamp>, with a numeric suffix if two set-asides
// land in the same second, checking every name the set would take.
func freeAsidePath(dbPath, label string, at time.Time) (string, error) {
	stem := dbPath + "." + label + "-" + at.Format(stampLayout)
	for i := 1; i <= 100; i++ {
		base := stem
		if i > 1 {
			base = fmt.Sprintf("%s-%d", stem, i)
		}
		free := true
		for _, s := range append([]string{""}, sidecars...) {
			if _, err := os.Lstat(base + s); err == nil {
				free = false
				break
			}
		}
		if free {
			return base, nil
		}
	}
	return "", fmt.Errorf("no free name to move %s aside to", dbPath)
}

// moveBack undoes setAside for the given paths, each of which ends in its suffix
// relative to the set-aside base name.
func moveBack(moved []string, dbPath string) error {
	if len(moved) == 0 {
		return nil
	}
	base := moved[0]
	for _, s := range sidecars {
		base = strings.TrimSuffix(base, s)
	}
	var errs []error
	for i := len(moved) - 1; i >= 0; i-- {
		suffix := strings.TrimPrefix(moved[i], base)
		if err := os.Rename(moved[i], dbPath+suffix); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkpoint copies every committed WAL frame into the main database file and
// truncates the WAL, using a connection of its own that is closed before returning
// (Windows cannot rename a file that is still open).
func checkpoint(ctx context.Context, dbPath string) error {
	// No journal_mode pragma here, unlike Open: this must not change the file's mode,
	// only fold in what is already committed. A short busy timeout, because the caller
	// holds the lock and nothing should be competing.
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1)

	var busy, logFrames, done int
	err = conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done)
	closeErr := conn.Close()
	switch {
	case err != nil:
		return err
	case busy != 0:
		return errors.New("another connection prevented the checkpoint from completing")
	case closeErr != nil:
		return closeErr
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
//
// The schedule is anchored to the newest snapshot on disk rather than to process
// start. A plain ticker only fired a full interval after start, so a server restarted
// more often than that (a deploy a day, say) never took a scheduled backup at all.
func BackupLoop(ctx context.Context, sqlDB *sql.DB, cfg BackupConfig, every time.Duration) {
	if every <= 0 {
		every = DefaultBackupEvery
	}
	wait := firstBackupDelay(cfg.Dir, every, time.Now())
	log.Printf("backups: next snapshot in %s", wait.Round(time.Second))

	t := time.NewTimer(wait)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		next := every
		snap, err := Backup(ctx, sqlDB, cfg)
		switch {
		case err != nil && ctx.Err() != nil:
			return // shutting down mid-backup is not a failure worth shouting about
		case err != nil:
			// Loud, and every time. A backup that quietly stopped working two months ago is
			// worse than none at all, because it removed the worry without removing the risk.
			next = min(every, backupRetryAfter)
			log.Printf("BACKUP FAILED: %v (retrying in %s)", err, next)
		default:
			log.Printf("backup: %s (%s) verified in %s%s",
				filepath.Base(snap.Path), humanBytes(snap.Bytes),
				snap.Took.Round(time.Millisecond), prunedNote(snap.Pruned))
		}
		t.Reset(next)
	}
}

// firstBackupDelay is how long BackupLoop waits before its first snapshot: until the
// newest snapshot is an interval old, but never less than backupStartDelay, and
// immediately-ish (backupStartDelay) when there is no snapshot or it is overdue.
func firstBackupDelay(dir string, every time.Duration, now time.Time) time.Duration {
	newest, ok := newestSnapshotTime(dir)
	if !ok {
		return backupStartDelay
	}
	wait := newest.Add(every).Sub(now)
	switch {
	case wait < backupStartDelay:
		return backupStartDelay
	case wait > every:
		// A snapshot stamped in the future means the clock moved; do not wait longer
		// than one interval because of it.
		return every
	}
	return wait
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

// BackupDirFor is where snapshots go when nothing configures a directory: "backups"
// beside the database file.
//
// This used to be os.UserCacheDir(), which depends on who is asking. Under systemd
// with User=yaba the server wrote to ~yaba/.cache, while `yaba restore` run by an
// admin looked in the admin's own cache and found nothing -- and a cache directory is
// by definition something the system may clean. A path derived from the database is
// the same for every user and every process that agrees on the database. Production
// should still set YABA_BACKUP_DIR (ideally another disk) and copy snapshots off the
// machine, because a backup beside its original shares its fate.
func BackupDirFor(dbPath string) string {
	dir := filepath.Dir(dbPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "backups")
}

// DefaultBackupDir is BackupDirFor applied to YABA_DB (or "yaba.db", the default both
// binaries use), for a flag default computed before flags are parsed. A caller that
// knows its database path after parsing should prefer BackupDirFor(thatPath), so a
// -db flag moves the default with it.
func DefaultBackupDir() string {
	dbPath := strings.TrimSpace(os.Getenv("YABA_DB"))
	if dbPath == "" {
		dbPath = "yaba.db"
	}
	return BackupDirFor(dbPath)
}

// ═════════════════════════════════════════════════════════════════════════════
// lock
// ═════════════════════════════════════════════════════════════════════════════

// ErrLocked means another process holds the database's lock: normally the running
// server, sometimes a second maintenance command.
var ErrLocked = errors.New("the database is in use by another process")

// Lock is an exclusive, advisory, whole-process hold on a database, taken by the
// server for its lifetime and by the maintenance commands that replace or rewrite the
// database underneath it (restore, reset, repair).
//
// Why a lock file rather than asking SQLite. In WAL mode an idle server connection
// holds no lock on the database file at all -- only a shared lock on a byte of the
// -shm -- so neither BEGIN EXCLUSIVE nor locking_mode=EXCLUSIVE can see that a server
// is running between requests; they would succeed, and the server's next write would
// land in a file restore had already moved aside. A separate lock file avoids all of
// that, and is reliable on both platforms we run on:
//
//   - Linux/macOS use flock(2). The kernel drops it when the process exits for any
//     reason, kill -9 and OOM included, so there is never a stale lock to clean up.
//     It is per open file, so it also excludes a second holder in the same process,
//     unlike POSIX fcntl locks, which SQLite uses on the database file itself and
//     which this deliberately does not touch.
//   - Windows uses LockFileEx, which has the same released-on-exit behaviour.
//
// The file itself is never deleted (unlinking a lock file races with the next
// opener) and is empty. It is opened read-only, which both APIs accept, and created
// 0644, so a lock file first created by root through `sudo yaba restore` can still be
// locked by the service account afterwards.
type Lock struct {
	f    *os.File
	path string
}

// LockPath is the lock file for the database at dbPath.
func LockPath(dbPath string) string { return dbPath + ".lock" }

// AcquireLock takes the database's lock without waiting, and returns an error
// wrapping ErrLocked if another process holds it. Release it when done; the operating
// system releases it anyway if the process dies.
func AcquireLock(dbPath string) (*Lock, error) {
	path := LockPath(dbPath)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	held, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if held {
		f.Close()
		return nil, fmt.Errorf("%w (%s is locked)", ErrLocked, path)
	}
	return &Lock{f: f, path: path}, nil
}

// Release gives the lock up. It is safe to call more than once, and on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockFile(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
