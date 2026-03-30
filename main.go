package main

import (
	"database/sql"
	"log"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	_ "modernc.org/sqlite"

	"github.com/jthomasw/YABA-2026/foo"
	"github.com/jthomasw/YABA-2026/http"
	"github.com/jthomasw/YABA-2026/sqlite"
)

type uuidGenerator struct{}

func (u *uuidGenerator) GenerateId() (string, error) {
	return uuid.New().String(), nil
}

func main() {
	var err error

	// Open SQLite database
	db, err := sql.Open("sqlite", "yaba.db")
	if err != nil {
		log.Fatal("Database open error:", err)
	}

	// Test DB connection
	if err = db.Ping(); err != nil {
		log.Fatal("Database connection failed:", err)
	}

	log.Println("Database connected successfully")

	createTables(db)

	// Initialize components
	sqliteClient, err := sqlite.NewClient("yaba.db")
	if err != nil {
		log.Fatal("SQLite client error:", err)
	}

	fooService := foo.NewService(foo.ServiceAttachments{
		BarRepository: &sqliteClient,
		IdGenerator:   &uuidGenerator{},
	})

	store := sessions.NewCookieStore([]byte("super-secret-key"))

	server := http.NewServer(http.ServerAttachments{
		FooService: fooService,
		DB:         db,
		Store:      store,
	})

	log.Println("Server running at http://localhost:8080")
	log.Fatal(server.ListenAndServe())
}

func createTables(db *sql.DB) {
	userQuery := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password TEXT NOT NULL
	);`

	_, err := db.Exec(userQuery)
	if err != nil {
		log.Fatal("Users table creation failed:", err)
	}

	barQuery := `
	CREATE TABLE IF NOT EXISTS bar (
		id TEXT PRIMARY KEY,
		b INTEGER,
		a INTEGER,
		r INTEGER
	);`

	_, err = db.Exec(barQuery)
	if err != nil {
		log.Fatal("Bar table creation failed:", err)
	}

	log.Println("Tables ready")
}
