package http

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
)

type ServerAttachments struct {
	DB    *sql.DB
	Store *sessions.CookieStore
}

func NewServer(att ServerAttachments) http.Server {

	mux := http.NewServeMux()

	mux.HandleFunc("/", loginPage)
	mux.HandleFunc("/register", registerUser(att.DB))
	mux.HandleFunc("/login", loginUser(att.DB, att.Store))
	mux.HandleFunc("/dashboard", auth(dashboard(att.DB, att.Store), att.Store))
	mux.HandleFunc("/add-income", auth(addIncome(att.DB, att.Store), att.Store))
	mux.HandleFunc("/add-expense", auth(addExpense(att.DB, att.Store), att.Store))
	mux.HandleFunc("/logout", logout(att.Store))
	mux.HandleFunc("/transactions", auth(viewTransactions(att.DB, att.Store), att.Store))
	mux.HandleFunc("/delete-transaction", auth(deleteTransaction(att.DB, att.Store), att.Store))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	return http.Server{
		Addr:    ":8000",
		Handler: mux,
	}
}

func auth(next http.HandlerFunc, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")

		log.Println("AUTH CHECK USER:", session.Values["user"])

		if session.Values["user"] == nil {
			log.Println("REDIRECTING TO LOGIN ❌")
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Println("USER AUTHORIZED ✅")
		next(w, r)
	}
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	template.Must(template.ParseFiles("templates/login.html")).Execute(w, nil)
}

func registerUser(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			template.Must(template.ParseFiles("templates/register.html")).Execute(w, nil)
			return
		}

		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/register", http.StatusSeeOther)
			return
		}

		r.ParseForm()
		username := r.FormValue("username")
		password := r.FormValue("password")

		if username == "" || password == "" {
			http.Error(w, "Username and password required", http.StatusBadRequest)
			return
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "Could not hash password", http.StatusInternalServerError)
			return
		}

		_, err = db.Exec(
			"INSERT INTO users(username, password) VALUES(?, ?)",
			username,
			string(hashedPassword),
		)
		if err != nil {
			log.Println("REGISTER ERROR:", err)
			http.Error(w, "Could not register user", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func loginUser(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		r.ParseForm()
		username := r.FormValue("username")
		password := r.FormValue("password")

		var stored string
		err := db.QueryRow("SELECT password FROM users WHERE username=?", username).Scan(&stored)
		if err != nil {
			http.Error(w, "Invalid login", 401)
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) != nil {
			http.Error(w, "Invalid login", 401)
			return
		}

		session, _ := store.Get(r, "session")
		session.Values["user"] = username
		err = session.Save(r, w)
		if err != nil {
		log.Println("SESSION SAVE ERROR:", err)
		}

		log.Println("LOGIN SAVED USER:", session.Values["user"])

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

func dashboard(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Println("DASHBOARD USER:", user)

		var income float64
		var expense float64

		db.QueryRow("SELECT IFNULL(SUM(amount),0) FROM income WHERE user=?", user).Scan(&income)
		db.QueryRow("SELECT IFNULL(SUM(amount),0) FROM expense WHERE user=?", user).Scan(&expense)

		current := income - expense

		log.Println("Income:", income)
		log.Println("Expense:", expense)
		log.Println("Current:", current)

		data := map[string]interface{}{
			"Username":     user,
			"CurrentFunds": current,
		}

		t := template.Must(template.ParseFiles(
			"templates/dashboard.html",
			"templates/dashboard_current.html",
			"templates/dashboard_emergency.html",
			"templates/dashboard_income.html",
			"templates/dashboard_expenses.html",
		))

		err := t.Execute(w, data)
		if err != nil {
			log.Println("DASHBOARD TEMPLATE ERROR:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

func addIncome(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodGet {
			template.Must(template.ParseFiles("templates/add_income.html")).Execute(w, nil)
			return
		}

		r.ParseForm()

		source := r.FormValue("source")
		date := r.FormValue("date")
		amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)

		log.Println("INSERT INCOME:", user, amount)

		_, err := db.Exec(
			"INSERT INTO income(user, source, date, amount) VALUES(?,?,?,?)",
			user, source, date, amount,
		)

		if err != nil {
			log.Println("ERROR:", err)
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

func addExpense(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodGet {
			template.Must(template.ParseFiles("templates/add_expense.html")).Execute(w, nil)
			return
		}

		r.ParseForm()

		category := r.FormValue("category")
		date := r.FormValue("date")
		amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)

		log.Println("INSERT EXPENSE:", user, amount)

		db.Exec(
			"INSERT INTO expense(user, source, date, amount) VALUES(?,?,?,?)",
			user, category, date, amount,
		)

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}
func viewTransactions(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Println("INSIDE TRANSACTIONS PAGE ✅")

		filter := r.URL.Query().Get("type")

		type Transaction struct {
			ID     int
			Type   string
			Name   string
			Date   string
			Amount float64
		}

		var transactions []Transaction

		// ✅ INCOME
		if filter == "" || filter == "income" {
			rows, err := db.Query(`
				SELECT id, source, date, amount 
				FROM income 
				WHERE user=? 
				ORDER BY date DESC`, user)

			if err != nil {
				log.Println("INCOME QUERY ERROR:", err)
			} else {
				defer rows.Close()

				for rows.Next() {
					var t Transaction
					err := rows.Scan(&t.ID, &t.Name, &t.Date, &t.Amount)
					if err == nil {
						t.Type = "Income"
						transactions = append(transactions, t)
					}
				}
			}
		}

		// ✅ EXPENSE (FIXED COLUMN NAME)
		if filter == "" || filter == "expense" {
			rows, err := db.Query(`
				SELECT id, source, date, amount 
				FROM expense 
				WHERE user=? 
				ORDER BY date DESC`, user)

			if err != nil {
				log.Println("EXPENSE QUERY ERROR:", err)
			} else {
				defer rows.Close()

				for rows.Next() {
					var t Transaction
					err := rows.Scan(&t.ID, &t.Name, &t.Date, &t.Amount)
					if err == nil {
						t.Type = "Expense"
						transactions = append(transactions, t)
					}
				}
			}
		}

		data := map[string]interface{}{
			"Transactions": transactions,
			"Username":     user,
			"Filter":       filter,
		}

		template.Must(template.ParseFiles("templates/transactions.html")).Execute(w, data)
	}
}

func deleteTransaction(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		id := r.URL.Query().Get("id")
		tType := r.URL.Query().Get("type")

		if tType == "income" {
			db.Exec("DELETE FROM income WHERE id=? AND user=?", id, user)
		} else if tType == "expense" {
			db.Exec("DELETE FROM expense WHERE id=? AND user=?", id, user)
		}

		http.Redirect(w, r, "/transactions", http.StatusSeeOther)
	}
}

func logout(store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		session.Options.MaxAge = -1
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
