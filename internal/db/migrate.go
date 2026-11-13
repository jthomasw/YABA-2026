package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
)

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

// LatestVersion is the newest schema version this build can migrate to.
func LatestVersion() int {
	latest := 0
	for _, m := range migrations() {
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest
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

		// Migration 24 gives a monthly bill the day of the month it is due, so
		// the planner can say when rent is next due the way recurring income
		// says when the next paycheck lands. NULL means no day was set, which is
		// every existing bill.
		{Version: 24, Name: "monthly bills have a due day", Run: migrate024},

		// Migration 25 gives an invitation a secret, emailed as a link: opening
		// it proves the invited mailbox is yours. Without it, whoever first
		// registered the invited address could accept. Stored hashed, like
		// session and reset tokens. NULL is an invitation sent before this,
		// accepted the old way; they expire within a day.
		{Version: 25, Name: "invitations carry an emailed secret", Run: migrate025},
		{Version: 26, Name: "recurring expenses keep who, where and why", Run: migrate026},
	}
}

// migrate024 adds expense_buckets.due_day. Like migrate023 it checks first, so
// running it against a database that already has the column -- one whose
// recorded version was wound back -- is a no-op rather than a failure.
func migrate024(tx *sql.Tx) error {
	has, err := hasColumn(tx, "expense_buckets", "due_day")
	if err != nil || has {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE expense_buckets ADD COLUMN due_day INTEGER
		CHECK (due_day IS NULL OR due_day BETWEEN 1 AND 31)`); err != nil {
		return fmt.Errorf("add bill due day: %w", err)
	}
	return nil
}

// migrate025 adds household_invites.token_hash; a no-op if it is already there.
func migrate025(tx *sql.Tx) error {
	has, err := hasColumn(tx, "household_invites", "token_hash")
	if err != nil || has {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE household_invites ADD COLUMN token_hash TEXT`); err != nil {
		return fmt.Errorf("add invitation secret: %w", err)
	}
	return nil
}

// migrate026 gives recurring expenses the payee, place and note a one-time
// expense has, so what is typed on the form is kept and copied onto every
// expense the schedule creates. Each column is added only if missing.
func migrate026(tx *sql.Tx) error {
	for _, col := range []string{"payee", "place", "note"} {
		has, err := hasColumn(tx, "recurring_expense", col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE recurring_expense ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add recurring_expense.%s: %w", col, err)
		}
	}
	return nil
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
