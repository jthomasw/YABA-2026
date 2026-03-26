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
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE,
		password TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS income (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user TEXT,
		source TEXT,
		date TEXT,
		amount REAL
	)`)
}
