package db

import (
	"database/sql"
	"fmt"
	"log"
)

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
