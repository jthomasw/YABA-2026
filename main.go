package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var db *sql.DB
var store = sessions.NewCookieStore([]byte("super-secret-key"))

func main() {
	var err error

	// Open SQLite database
	db, err = sql.Open("sqlite", "yaba.db")
	if err != nil {
		log.Fatal("Database open error:", err)
	}

	// Test DB connection
	if err = db.Ping(); err != nil {
		log.Fatal("Database connection failed:", err)
	}

	log.Println("Database connected successfully")

	createTable()

	// Static files
	http.Handle("/static/",
		http.StripPrefix("/static/",
			http.FileServer(http.Dir("static"))))

	// Routes
	http.HandleFunc("/", loginPage)
	http.HandleFunc("/register", registerPage)
	http.HandleFunc("/login", loginUser)
	http.HandleFunc("/register-user", registerUser)
	http.HandleFunc("/dashboard", dashboard)
	http.HandleFunc("/logout", logout)

	log.Println("Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func createTable() {
	query := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password TEXT NOT NULL
	);`

	_, err := db.Exec(query)
	if err != nil {
		log.Fatal("Table creation failed:", err)
	}

	log.Println("Users table ready")
}

func registerPage(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("templates/register.html"))
	tmpl.Execute(w, nil)
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("templates/login.html"))
	tmpl.Execute(w, nil)
}

func registerUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/register", http.StatusSeeOther)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Form parsing error", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == "" || password == "" {
		http.Error(w, "Username and password required", http.StatusBadRequest)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Password hashing failed", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(
		"INSERT INTO users(username, password) VALUES(?, ?)",
		username,
		string(hashedPassword),
	)

	if err != nil {
		http.Error(w, "Username already taken", http.StatusBadRequest)
		return
	}

	log.Println("User registered:", username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func loginUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Form parsing error", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	var storedPassword string

	err = db.QueryRow(
		"SELECT password FROM users WHERE username = ?",
		username,
	).Scan(&storedPassword)

	if err != nil {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	err = bcrypt.CompareHashAndPassword(
		[]byte(storedPassword),
		[]byte(password),
	)

	if err != nil {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	session, _ := store.Get(r, "session")
	session.Values["user"] = username
	session.Save(r, w)

	log.Println("User logged in:", username)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func dashboard(w http.ResponseWriter, r *http.Request) {

	session, _ := store.Get(r, "session")
	user := session.Values["user"]

	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	tmpl := template.Must(template.ParseFiles("templates/dashboard.html"))
	tmpl.Execute(w, user)
}

func logout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	session.Options.MaxAge = -1
	session.Save(r, w)

	log.Println("User logged out")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
