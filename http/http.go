package http

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
	mux.HandleFunc("/add-emergency-deposit", auth(addEmergencyDeposit(att.DB, att.Store), att.Store))
	mux.HandleFunc("/add-emergency-withdrawal", auth(addEmergencyWithdrawal(att.DB, att.Store), att.Store))
	mux.HandleFunc("/set-emergency-goal", auth(setEmergencyGoal(att.DB, att.Store), att.Store))
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

		type ChartPoint struct {
			Date   string
			Amount float64
			Type   string
		}

		rows, err := db.Query(`
			SELECT date, amount, 'income' as type
			FROM income
			WHERE user=?

			UNION ALL

			SELECT date, amount, 'expense' as type
			FROM expense
			WHERE user=?

			ORDER BY date ASC
		`, user, user)

		if err != nil {
			log.Println("CHART QUERY ERROR:", err)
			http.Error(w, "Could not load dashboard chart data", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var labels []string
		var balances []float64
		var runningBalance float64 = 0

		for rows.Next() {
			var p ChartPoint
			err := rows.Scan(&p.Date, &p.Amount, &p.Type)
			if err != nil {
				log.Println("CHART SCAN ERROR:", err)
				continue
			}

			if p.Type == "income" {
				runningBalance += p.Amount
			} else if p.Type == "expense" {
				runningBalance -= p.Amount
			}

			labels = append(labels, p.Date)
			balances = append(balances, runningBalance)
		}

		labelsJSON, err := json.Marshal(labels)
		if err != nil {
			log.Println("LABELS JSON ERROR:", err)
			http.Error(w, "Could not prepare chart labels", http.StatusInternalServerError)
			return
		}

		balancesJSON, err := json.Marshal(balances)
		if err != nil {
			log.Println("BALANCES JSON ERROR:", err)
			http.Error(w, "Could not prepare chart values", http.StatusInternalServerError)
			return
		}

		var emergencyDeposits float64
		var emergencyWithdrawals float64
		var emergencyGoal float64
		var emergencyMonths int

		db.QueryRow("SELECT IFNULL(SUM(amount),0) FROM emergency_fund WHERE user=? AND type='deposit'", user).Scan(&emergencyDeposits)
		db.QueryRow("SELECT IFNULL(SUM(amount),0) FROM emergency_fund WHERE user=? AND type='withdrawal'", user).Scan(&emergencyWithdrawals)

		err = db.QueryRow("SELECT IFNULL(target_amount,0), IFNULL(months_target,0) FROM emergency_goals WHERE user=?", user).Scan(&emergencyGoal, &emergencyMonths)
		if err != nil && err != sql.ErrNoRows {
			log.Println("EMERGENCY GOAL QUERY ERROR:", err)
		}

		emergencyBalance := emergencyDeposits - emergencyWithdrawals

		var monthlyWithdrawalRate float64
		rows2, err := db.Query(`
			SELECT strftime('%Y-%m', date) as month, SUM(amount) as total
			FROM emergency_fund
			WHERE user=? AND type='withdrawal'
			GROUP BY strftime('%Y-%m', date)
			ORDER BY month DESC
			LIMIT 3
		`, user)
		if err == nil {
			defer rows2.Close()

			var rates []float64
			for rows2.Next() {
				var month string
				var total float64
				err := rows2.Scan(&month, &total)
				if err == nil {
					rates = append(rates, total)
				}
			}

			if len(rates) > 0 {
				sum := 0.0
				for _, rate := range rates {
					sum += rate
				}
				monthlyWithdrawalRate = sum / float64(len(rates))
			}
		} else {
			log.Println("EMERGENCY WITHDRAWAL RATE QUERY ERROR:", err)
		}

		var monthsRemaining float64
		if monthlyWithdrawalRate > 0 {
			monthsRemaining = emergencyBalance / monthlyWithdrawalRate
		} else {
			monthsRemaining = -1
		}

		data := map[string]interface{}{
			"Username":              user,
			"CurrentFunds":          current,
			"ChartLabels":           template.JS(labelsJSON),
			"ChartBalances":         template.JS(balancesJSON),
			"EmergencyBalance":      emergencyBalance,
			"EmergencyGoal":         emergencyGoal,
			"EmergencyMonthsTarget": emergencyMonths,
			"MonthlyWithdrawalRate": monthlyWithdrawalRate,
			"MonthsRemaining":       monthsRemaining,
		}

		t := template.Must(template.ParseFiles(
			"templates/dashboard.html",
			"templates/dashboard_current.html",
			"templates/dashboard_emergency.html",
			"templates/dashboard_income.html",
			"templates/dashboard_expenses.html",
		))

		var buf bytes.Buffer
		err = t.Execute(&buf, data)
		if err != nil {
			log.Println("DASHBOARD TEMPLATE ERROR:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		_, err = buf.WriteTo(w)
		if err != nil {
			log.Println("DASHBOARD WRITE ERROR:", err)
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

		_, err := db.Exec(
			"INSERT INTO expense(user, category, date, amount) VALUES(?,?,?,?)",
			user, category, date, amount,
		)
		if err != nil {
			log.Println("ERROR:", err)
		}

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

		if filter == "" || filter == "expense" {
			rows, err := db.Query(`
				SELECT id, category, date, amount 
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

func addEmergencyDeposit(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodPost {
			r.ParseForm()
			amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
			date := r.FormValue("date")

			if amount > 0 {
				_, err := db.Exec(
					"INSERT INTO emergency_fund(user, date, amount, type) VALUES(?,?,?,'deposit')",
					user, date, amount,
				)
				if err != nil {
					log.Println("EMERGENCY DEPOSIT ERROR:", err)
				}
			}
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

func addEmergencyWithdrawal(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodPost {
			r.ParseForm()
			amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
			date := r.FormValue("date")

			if amount > 0 {
				_, err := db.Exec(
					"INSERT INTO emergency_fund(user, date, amount, type) VALUES(?,?,?,'withdrawal')",
					user, date, amount,
				)
				if err != nil {
					log.Println("EMERGENCY WITHDRAWAL ERROR:", err)
				}
			}
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

func setEmergencyGoal(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)

		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodPost {
			r.ParseForm()
			targetAmount, _ := strconv.ParseFloat(r.FormValue("target_amount"), 64)
			monthsTarget, _ := strconv.Atoi(r.FormValue("months_target"))

			_, err := db.Exec(
				"INSERT OR REPLACE INTO emergency_goals(user, target_amount, months_target) VALUES(?,?,?)",
				user, targetAmount, monthsTarget,
			)
			if err != nil {
				log.Println("EMERGENCY GOAL ERROR:", err)
			}
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}