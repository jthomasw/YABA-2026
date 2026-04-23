package http

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
)

type ServerAttachments struct {
	DB    *sql.DB
	Store *sessions.CookieStore
}

func createFund(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			r.ParseForm()
			name := r.FormValue("name")
			goalStr := r.FormValue("goal")
			goal := 0.0
			if goalStr != "" {
				if g, err := strconv.ParseFloat(goalStr, 64); err == nil {
					goal = g
				}
			}
			if name != "" {
				_, err := db.Exec("INSERT INTO funds(user, name, balance, goal) VALUES(?, ?, 0, ?)", user, name, goal)
				if err != nil {
					log.Println("CREATE FUND error:", err)
				}
			}
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// Handler to update a fund's goal
func updateFundGoal(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			r.ParseForm()
			fundID, _ := strconv.Atoi(r.FormValue("fund_id"))
			goal, _ := strconv.ParseFloat(r.FormValue("goal"), 64)
			if fundID > 0 {
				_, err := db.Exec("UPDATE funds SET goal = ? WHERE id = ? AND user = ?", goal, fundID, user)
				if err != nil {
					log.Println("UPDATE FUND GOAL error:", err)
				}
			}
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// Handler to add to a fund (transfer from current funds)
func addToFund(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			r.ParseForm()
			fundID, _ := strconv.Atoi(r.FormValue("fund_id"))
			amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
			date := today()
			if fundID > 0 && amount > 0 {
				// Deduct from current funds (add expense)
				_, err1 := db.Exec("INSERT INTO expense(user, date, amount, category) VALUES(?, ?, ?, ?)", user, date, amount, "Emergency Fund")
				// Add to fund balance
				_, err2 := db.Exec("UPDATE funds SET balance = balance + ? WHERE id = ? AND user = ?", amount, fundID, user)
				// Log fund transaction
				_, err3 := db.Exec("INSERT INTO fund_transactions(user, fund_id, date, amount, type) VALUES(?, ?, ?, ?, ?)", user, fundID, date, amount, "deposit")
				if err1 != nil || err2 != nil || err3 != nil {
					log.Println("ADD TO FUND error:", err1, err2, err3)
				}
			}
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
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
	// New endpoints for funds
	mux.HandleFunc("/create-fund", auth(createFund(att.DB, att.Store), att.Store))
	mux.HandleFunc("/add-to-fund", auth(addToFund(att.DB, att.Store), att.Store))
	mux.HandleFunc("/update-fund-goal", auth(updateFundGoal(att.DB, att.Store), att.Store))

	mux.HandleFunc("/logout", logout(att.Store))
	mux.HandleFunc("/transactions", auth(viewTransactions(att.DB, att.Store), att.Store))
	mux.HandleFunc("/delete-transaction", auth(deleteTransaction(att.DB, att.Store), att.Store))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/upload-receipt", auth(uploadReceipt(att.DB, att.Store), att.Store))

	return http.Server{
		Addr:    ":8000",
		Handler: mux,
	}
}

// today returns the current date as YYYY-MM-DD.
func today() string {
	return time.Now().Format("2006-01-02")
}

// fallbackDate returns d if non-empty, otherwise today's date.
func fallbackDate(d string) string {
	if d == "" {
		return today()
	}
	return d
}

func auth(next http.HandlerFunc, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		if session.Values["user"] == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
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
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "Could not hash password", http.StatusInternalServerError)
			return
		}
		_, err = db.Exec("INSERT INTO users(username, password) VALUES(?,?)", username, string(hash))
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
			http.Error(w, "Invalid login", http.StatusUnauthorized)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) != nil {
			http.Error(w, "Invalid login", http.StatusUnauthorized)
			return
		}
		session, _ := store.Get(r, "session")
		session.Values["user"] = username
		if err = session.Save(r, w); err != nil {
			log.Println("SESSION SAVE ERROR:", err)
		}
		log.Println("LOGIN OK:", username)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
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

// ── DASHBOARD ──────────────────────────────────────────────────────────────────
// Slide 17-18: currentFunds = SUM(income) - SUM(expense)
// Every load re-queries the DB so adding income/expense is immediately reflected.
func dashboard(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// ── Step 1: total income and expense ─────────────────────────────────
		var totalIncome float64
		var totalExpense float64

		err := db.QueryRow(
			"SELECT IFNULL(SUM(amount), 0) FROM income WHERE user = ?", user,
		).Scan(&totalIncome)
		if err != nil {
			log.Printf("DASHBOARD income SUM error (user=%q): %v", user, err)
		}

		err = db.QueryRow(
			"SELECT IFNULL(SUM(amount), 0) FROM expense WHERE user = ?", user,
		).Scan(&totalExpense)
		if err != nil {
			log.Printf("DASHBOARD expense SUM error (user=%q): %v", user, err)
		}

		// currentFunds = SUM(income) - SUM(expense)
		currentFunds := totalIncome - totalExpense
		log.Printf("DASHBOARD user=%q  income=%.2f  expense=%.2f  currentFunds=%.2f",
			user, totalIncome, totalExpense, currentFunds)

		// ── Step 2: running-balance chart ────────────────────────────────────
		// Combine income (+) and expense (-) sorted by date to build a
		// running balance timeline for the line chart.
		var chartLabels []string
		var chartBalances []float64

		chartRows, err := db.Query(`
			SELECT COALESCE(date, ?) AS d, amount, 'income' AS ttype
			FROM income
			WHERE user = ?
			UNION ALL
			SELECT COALESCE(date, ?) AS d, amount, 'expense' AS ttype
			FROM expense
			WHERE user = ?
			ORDER BY d ASC
		`, today(), user, today(), user)

		if err != nil {
			log.Println("DASHBOARD chart query error:", err)
		} else {
			running := 0.0
			for chartRows.Next() {
				var d string
				var amount float64
				var ttype string
				if scanErr := chartRows.Scan(&d, &amount, &ttype); scanErr != nil {
					log.Println("DASHBOARD chart scan error:", scanErr)
					continue
				}
				if ttype == "income" {
					running += amount
				} else {
					running -= amount
				}
				chartLabels = append(chartLabels, d)
				chartBalances = append(chartBalances, running)
			}
			chartRows.Close()
		}

		labelsJSON, _ := json.Marshal(chartLabels)
		balancesJSON, _ := json.Marshal(chartBalances)

		// ── Step 3: income breakdown by source (pie chart) ───────────────────
		var incomeSourceLabels []string
		var incomeSourceTotals []float64

		srcRows, err := db.Query(`
			SELECT source, SUM(amount) AS total
			FROM income
			WHERE user = ?
			GROUP BY 1
			ORDER BY 2 DESC
		`, user)
		if err != nil {
			log.Println("DASHBOARD income source query error:", err)
		} else {
			for srcRows.Next() {
				var src string
				var total float64
				if scanErr := srcRows.Scan(&src, &total); scanErr == nil {
					incomeSourceLabels = append(incomeSourceLabels, src)
					incomeSourceTotals = append(incomeSourceTotals, total)
				}
			}
			srcRows.Close()
		}

		incomeSourceLabelsJSON, _ := json.Marshal(incomeSourceLabels)
		incomeSourceTotalsJSON, _ := json.Marshal(incomeSourceTotals)

		// ── Step 3b: expense breakdown by category (pie chart) ───────────────
		expenseCategoryLabels := []string{}
		expenseCategoryTotals := []float64{}

		expCatRows, err := db.Query(`
			SELECT IFNULL(NULLIF(TRIM(category),''), 'Uncategorised'),
			       SUM(amount)
			FROM expense
			WHERE user = ?
			GROUP BY IFNULL(NULLIF(TRIM(category),''), 'Uncategorised')
			ORDER BY SUM(amount) DESC
		`, user)
		if err != nil {
			log.Println("DASHBOARD expense category query error:", err)
		} else {
			for expCatRows.Next() {
				var cat string
				var total float64
				if scanErr := expCatRows.Scan(&cat, &total); scanErr == nil {
					log.Printf("EXPENSE CATEGORY: cat=%q total=%.2f", cat, total)
					expenseCategoryLabels = append(expenseCategoryLabels, cat)
					expenseCategoryTotals = append(expenseCategoryTotals, total)
				}
			}
			expCatRows.Close()
		}
		log.Printf("EXPENSE CHART DATA: labels=%v totals=%v", expenseCategoryLabels, expenseCategoryTotals)

		expenseCategoryLabelsJSON, _ := json.Marshal(expenseCategoryLabels)
		expenseCategoryTotalsJSON, _ := json.Marshal(expenseCategoryTotals)

		// No legacy fundCategoryLabels/fundCategoryTotals. All fund data is passed as the Funds array for JS rendering.
		// ── Step 5: fetch funds and fund transactions for new UI ─────────────
		type Fund struct {
			ID      int
			Name    string
			Balance float64
			Goal    float64
		}
		var funds []Fund
		fundRows, err := db.Query("SELECT id, name, balance, goal FROM funds WHERE user = ? ORDER BY id ASC", user)
		if err == nil {
			for fundRows.Next() {
				var f Fund
				if scanErr := fundRows.Scan(&f.ID, &f.Name, &f.Balance, &f.Goal); scanErr == nil {
					funds = append(funds, f)
				}
			}
			fundRows.Close()
		}

		type FundTransaction struct {
			Date     string
			FundName string
			Amount   float64
			Type     string
		}
		var fundTransactions []FundTransaction
		txRows, err := db.Query(`SELECT ft.date, f.name, ft.amount, ft.type FROM fund_transactions ft JOIN funds f ON ft.fund_id = f.id WHERE ft.user = ? ORDER BY ft.date DESC, ft.id DESC LIMIT 50`, user)
		if err == nil {
			for txRows.Next() {
				var t FundTransaction
				if scanErr := txRows.Scan(&t.Date, &t.FundName, &t.Amount, &t.Type); scanErr == nil {
					fundTransactions = append(fundTransactions, t)
				}
			}
			txRows.Close()
		}

		// ── Step 6: pass all data to template ────────────────────────────────
		data := map[string]interface{}{
			"Username":              user,
			"CurrentFunds":          currentFunds,
			"TotalIncome":           totalIncome,
			"TotalExpense":          totalExpense,
			"ChartLabels":           template.JS(labelsJSON),
			"ChartBalances":         template.JS(balancesJSON),
			"IncomeSourceLabels":    template.JS(incomeSourceLabelsJSON),
			"IncomeSourceTotals":    template.JS(incomeSourceTotalsJSON),
			"ExpenseCategoryLabels": template.JS(expenseCategoryLabelsJSON),
			"ExpenseCategoryTotals": template.JS(expenseCategoryTotalsJSON),
			// New for emergency fund redesign:
			"Funds":            funds,
			"FundTransactions": fundTransactions,
		}

		t := template.Must(template.ParseFiles(
			"templates/dashboard.html",
			"templates/dashboard_current.html",
			"templates/dashboard_emergency.html",
			"templates/dashboard_income.html",
			"templates/dashboard_expenses.html",
		))

		var buf bytes.Buffer
		if err = t.Execute(&buf, data); err != nil {
			log.Println("DASHBOARD template error:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		buf.WriteTo(w)
	}
}

// ── ADD INCOME ─────────────────────────────────────────────────────────────────
// Slide 12: User fills form (source, date, amount) → POST /add-income
//
//	→ INSERT into income table → redirect to dashboard.
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
		date := fallbackDate(r.FormValue("date"))
		amountStr := r.FormValue("amount")

		amount, err := strconv.ParseFloat(amountStr, 64)
		if err != nil {
			// Cannot parse the number – go back to form.
			log.Printf("ADD INCOME parse error amount=%q: %v", amountStr, err)
			http.Redirect(w, r, "/add-income", http.StatusSeeOther)
			return
		}

		log.Printf("INSERT INCOME user=%q source=%q date=%q amount=%.2f", user, source, date, amount)

		_, err = db.Exec(
			"INSERT INTO income(user, source, date, amount) VALUES(?, ?, ?, ?)",
			user, source, date, amount,
		)
		if err != nil {
			log.Println("INSERT INCOME DB ERROR:", err)
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// ── ADD EXPENSE ────────────────────────────────────────────────────────────────
// Slide 16-18: User enters category, date, amount → POST /add-expense
//
//	→ INSERT into expense table → redirect to dashboard
//	→ dashboard recalculates currentFunds = SUM(income) - SUM(expense).
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
		date := fallbackDate(r.FormValue("date"))
		amountStr := r.FormValue("amount")

		amount, err := strconv.ParseFloat(amountStr, 64)
		if err != nil {
			// Cannot parse the number – go back to form.
			log.Printf("ADD EXPENSE parse error amount=%q: %v", amountStr, err)
			http.Redirect(w, r, "/add-expense", http.StatusSeeOther)
			return
		}

		log.Printf("INSERT EXPENSE user=%q category=%q date=%q amount=%.2f", user, category, date, amount)

		result, err := db.Exec(
			"INSERT INTO expense(user, category, date, amount) VALUES(?, ?, ?, ?)",
			user, category, date, amount,
		)
		if err != nil {
			log.Println("INSERT EXPENSE DB ERROR:", err)
		} else {
			newID, _ := result.LastInsertId()
			log.Printf("INSERT EXPENSE OK id=%d user=%q amount=%.2f", newID, user, amount)
		}

		// Redirect to dashboard so it recalculates current funds immediately.
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// ── UPLOAD RECEIPT ─────────────────────────────────────────────────────────────
// Slide 23: Upload receipt image → save file → create placeholder expense entry
//
//	so it immediately appears in current funds and transactions.
func uploadReceipt(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/add-expense", http.StatusSeeOther)
			return
		}

		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if err := r.ParseMultipartForm(5 << 20); err != nil {
			http.Error(w, "File too large (max 5 MB)", http.StatusBadRequest)
			return
		}

		file, handler, err := r.FormFile("receipt")
		if err != nil {
			log.Println("UPLOAD RECEIPT form file error:", err)
			http.Error(w, "File upload error", http.StatusBadRequest)
			return
		}
		defer file.Close()

		os.MkdirAll("./uploads", os.ModePerm)
		filename := time.Now().Format("20060102150405") + "_" + user + "_" + handler.Filename
		filePath := filepath.Join("./uploads", filename)

		dst, err := os.Create(filePath)
		if err != nil {
			log.Println("UPLOAD RECEIPT create file error:", err)
			http.Error(w, "Could not save file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err = io.Copy(dst, file); err != nil {
			log.Println("UPLOAD RECEIPT copy error:", err)
			http.Error(w, "Save failed", http.StatusInternalServerError)
			return
		}
		log.Println("UPLOAD RECEIPT saved:", filePath)

		// Insert a placeholder expense so the receipt is visible everywhere.
		_, dbErr := db.Exec(
			"INSERT INTO expense(user, category, date, amount) VALUES(?, ?, ?, ?)",
			user, "Scanned Expense ("+handler.Filename+")", today(), 0,
		)
		if dbErr != nil {
			log.Println("UPLOAD RECEIPT expense insert error:", dbErr)
		} else {
			log.Println("UPLOAD RECEIPT placeholder expense inserted")
		}

		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// ── TRANSACTIONS ───────────────────────────────────────────────────────────────
// Shows all transactions (income + expense) combined and sorted by date DESC.
// Filter: ?type=income  →  income only
//
//	?type=expense →  expense only
//	(none)        →  both combined
func viewTransactions(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user, ok := session.Values["user"].(string)
		if !ok || user == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		filter := r.URL.Query().Get("type")

		type Transaction struct {
			ID     int
			Type   string
			Name   string
			Date   string
			Amount float64
		}

		var transactions []Transaction
		now := today()

		switch filter {

		case "income":
			rows, err := db.Query(`
				SELECT id, source, COALESCE(NULLIF(date,''), ?) AS d, amount
				FROM income
				WHERE user = ?
				ORDER BY d DESC
			`, now, user)
			if err != nil {
				log.Println("TRANSACTIONS income query error:", err)
				break
			}
			for rows.Next() {
				var t Transaction
				if err := rows.Scan(&t.ID, &t.Name, &t.Date, &t.Amount); err == nil {
					t.Type = "Income"
					transactions = append(transactions, t)
				}
			}
			rows.Close()

		case "expense":
			rows, err := db.Query(`
				SELECT id, category, COALESCE(NULLIF(date,''), ?) AS d, amount
				FROM expense
				WHERE user = ?
				ORDER BY d DESC
			`, now, user)
			if err != nil {
				log.Println("TRANSACTIONS expense query error:", err)
				break
			}
			for rows.Next() {
				var t Transaction
				if err := rows.Scan(&t.ID, &t.Name, &t.Date, &t.Amount); err == nil {
					t.Type = "Expense"
					transactions = append(transactions, t)
				}
			}
			rows.Close()

		default:
			// Combine both tables, sort together by date descending.
			rows, err := db.Query(`
				SELECT id, 'Income' AS type, source AS name,
				       COALESCE(NULLIF(date,''), ?) AS d, amount
				FROM income
				WHERE user = ?
				UNION ALL
				SELECT id, 'Expense' AS type, category AS name,
				       COALESCE(NULLIF(date,''), ?) AS d, amount
				FROM expense
				WHERE user = ?
				ORDER BY d DESC
			`, now, user, now, user)
			if err != nil {
				log.Println("TRANSACTIONS combined query error:", err)
				break
			}
			for rows.Next() {
				var t Transaction
				if err := rows.Scan(&t.ID, &t.Type, &t.Name, &t.Date, &t.Amount); err == nil {
					transactions = append(transactions, t)
				}
			}
			rows.Close()
		}

		data := map[string]interface{}{
			"Transactions": transactions,
			"Username":     user,
			"Filter":       filter,
		}
		template.Must(template.ParseFiles("templates/transactions.html")).Execute(w, data)
	}
}

// ── DELETE TRANSACTION ─────────────────────────────────────────────────────────
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
		filterBack := r.URL.Query().Get("filter")

		if tType == "income" {
			_, err := db.Exec("DELETE FROM income WHERE id = ? AND user = ?", id, user)
			if err != nil {
				log.Println("DELETE INCOME error:", err)
			}
		} else if tType == "expense" {
			_, err := db.Exec("DELETE FROM expense WHERE id = ? AND user = ?", id, user)
			if err != nil {
				log.Println("DELETE EXPENSE error:", err)
			}
		}

		if filterBack != "" {
			http.Redirect(w, r, "/transactions?type="+filterBack, http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/transactions", http.StatusSeeOther)
		}
	}
}

// ── EMERGENCY FUND ─────────────────────────────────────────────────────────────
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
			date := fallbackDate(r.FormValue("date"))
			if amount > 0 {
				_, err := db.Exec(
					"INSERT INTO emergency_fund(user, date, amount, type) VALUES(?, ?, ?, 'deposit')",
					user, date, amount,
				)
				if err != nil {
					log.Println("EMERGENCY DEPOSIT error:", err)
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
			date := fallbackDate(r.FormValue("date"))
			if amount > 0 {
				_, err := db.Exec(
					"INSERT INTO emergency_fund(user, date, amount, type) VALUES(?, ?, ?, 'withdrawal')",
					user, date, amount,
				)
				if err != nil {
					log.Println("EMERGENCY WITHDRAWAL error:", err)
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
				"INSERT OR REPLACE INTO emergency_goals(user, target_amount, months_target) VALUES(?, ?, ?)",
				user, targetAmount, monthsTarget,
			)
			if err != nil {
				log.Println("EMERGENCY GOAL error:", err)
			}
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}
