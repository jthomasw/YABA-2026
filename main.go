package main

import (
	"database/sql"
	"log"

	nethttp "net/http"

	"github.com/gorilla/sessions"
	_ "modernc.org/sqlite"

	"github.com/jthomasw/YABA-2026/http"
)

func main() {

	// DB
	db, err := sql.Open("sqlite", "yaba.db")
	if err != nil {
		log.Fatal(err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal(err)
	}

	createTables(db)
	runMigrations(db) // ← apply schema changes to existing DB

	// SESSION
	store := sessions.NewCookieStore([]byte("super-secret-key"))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
		Secure:   false,
		SameSite: nethttp.SameSiteLaxMode,
	}

	// SERVER
	server := http.NewServer(http.ServerAttachments{
		DB:    db,
		Store: store,
	})

	log.Println("Running at http://localhost:8000")
	log.Fatal(server.ListenAndServe())
}

func createTables(db *sql.DB) {

	db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE,
		password TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS income (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		user   TEXT,
		source TEXT,
		date   TEXT,
		amount REAL
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS expense (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		user     TEXT,
		category TEXT,
		date     TEXT,
		amount   REAL
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS emergency_fund (
	    id     INTEGER PRIMARY KEY AUTOINCREMENT,
	    user   TEXT,
	    date   TEXT,
	    amount REAL,
	    type   TEXT
	)`)

	// New: Table for multiple funds
	db.Exec(`CREATE TABLE IF NOT EXISTS funds (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		user    TEXT,
		name    TEXT,
		balance REAL DEFAULT 0,
		goal    REAL DEFAULT 0
	)`)

	// New: Table for fund transactions
	db.Exec(`CREATE TABLE IF NOT EXISTS fund_transactions (
	    id      INTEGER PRIMARY KEY AUTOINCREMENT,
	    user    TEXT,
	    fund_id INTEGER,
	    date    TEXT,
	    amount  REAL,
	    type    TEXT,
	    FOREIGN KEY(fund_id) REFERENCES funds(id)
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS emergency_goals (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user          TEXT UNIQUE,
		target_amount REAL,
		months_target INTEGER
	)`)
}

// runMigrations adds columns that may be missing on databases that were
// created before the current schema was defined.
// ALTER TABLE ... ADD COLUMN is intentionally ignored when the column
// already exists — SQLite returns an error we safely discard.
func runMigrations(db *sql.DB) {
	// Add goal column to funds if missing
	var err error
	_, err = db.Exec(`ALTER TABLE funds ADD COLUMN goal REAL DEFAULT 0`)
	if err != nil {
		log.Println("migration funds.goal (already exists or added):", err)
	} else {
		log.Println("migration: added funds.goal column ✓")
	}

	// expense.category was added after the initial release.
	// Any DB created without it will fail every INSERT/SELECT that names it.
	_, err = db.Exec(`ALTER TABLE expense ADD COLUMN category TEXT DEFAULT ''`)
	if err != nil {
		// "duplicate column name" means it already exists — that is fine.
		log.Println("migration expense.category (already exists or added):", err)
	} else {
		log.Println("migration: added expense.category column ✓")
	}

	// income.source — guard the same way in case of older DBs.
	_, err = db.Exec(`ALTER TABLE income ADD COLUMN source TEXT DEFAULT ''`)
	if err != nil {
		log.Println("migration income.source (already exists or added):", err)
	} else {
		log.Println("migration: added income.source column ✓")
	}
}
