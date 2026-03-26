package http

import (
	"database/sql"
	"html/template"
	"log"
	nethttp "net/http"
	"strconv"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
)

type ServerAttachments struct {
	DB    *sql.DB
	Store *sessions.CookieStore
}

func NewServer(att ServerAttachments) nethttp.Server {

	mux := nethttp.NewServeMux()

	mux.HandleFunc("/", loginPage)
	mux.HandleFunc("/register", registerPage)
	mux.HandleFunc("/login", loginUser(att.DB, att.Store))
	mux.HandleFunc("/dashboard", auth(dashboard(att.Store), att.Store))
	mux.HandleFunc("/add-income", auth(addIncome(att.DB, att.Store), att.Store))
	mux.HandleFunc("/logout", logout(att.Store))

	mux.Handle("/static/", nethttp.StripPrefix("/static/", nethttp.FileServer(nethttp.Dir("static"))))

	return nethttp.Server{
		Addr:    ":8000",
		Handler: mux,
	}
}

func auth(next nethttp.HandlerFunc, store *sessions.CookieStore) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		session, _ := store.Get(r, "session")

		log.Println("SESSION:", session.Values["user"])

		if session.Values["user"] == nil {
			nethttp.Redirect(w, r, "/", nethttp.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func loginPage(w nethttp.ResponseWriter, r *nethttp.Request) {
	template.Must(template.ParseFiles("templates/login.html")).Execute(w, nil)
}

func registerPage(w nethttp.ResponseWriter, r *nethttp.Request) {
	template.Must(template.ParseFiles("templates/register.html")).Execute(w, nil)
}

func loginUser(db *sql.DB, store *sessions.CookieStore) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {

		if r.Method != nethttp.MethodPost {
			nethttp.Redirect(w, r, "/", nethttp.StatusSeeOther)
			return
		}

		r.ParseForm()
		username := r.FormValue("username")
		password := r.FormValue("password")

		var stored string
		err := db.QueryRow("SELECT password FROM users WHERE username=?", username).Scan(&stored)
		if err != nil {
			nethttp.Error(w, "Invalid login", 401)
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) != nil {
			nethttp.Error(w, "Invalid login", 401)
			return
		}

		session, _ := store.Get(r, "session")
		session.Values["user"] = username
		session.Save(r, w)

		log.Println("Logged in:", username)

		nethttp.Redirect(w, r, "/dashboard", nethttp.StatusSeeOther)
	}
}

func dashboard(store *sessions.CookieStore) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {

		session, _ := store.Get(r, "session")

		if session.Values["user"] == nil {
			nethttp.Redirect(w, r, "/", nethttp.StatusSeeOther)
			return
		}

		template.Must(template.ParseFiles("templates/dashboard.html")).Execute(w, nil)
	}
}

func addIncome(db *sql.DB, store *sessions.CookieStore) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {

		session, _ := store.Get(r, "session")
		user := session.Values["user"]

		if user == nil {
			nethttp.Redirect(w, r, "/", nethttp.StatusSeeOther)
			return
		}

		if r.Method == nethttp.MethodGet {
			template.Must(template.ParseFiles("templates/add_income.html")).Execute(w, nil)
			return
		}

		r.ParseForm()
		source := r.FormValue("source")
		date := r.FormValue("date")
		amountStr := r.FormValue("amount")

		amount, _ := strconv.ParseFloat(amountStr, 64)

		db.Exec("INSERT INTO income(user, source, date, amount) VALUES(?,?,?,?)",
			user, source, date, amount)

		nethttp.Redirect(w, r, "/dashboard", nethttp.StatusSeeOther)
	}
}

func logout(store *sessions.CookieStore) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		session, _ := store.Get(r, "session")
		session.Options.MaxAge = -1
		session.Save(r, w)

		nethttp.Redirect(w, r, "/", nethttp.StatusSeeOther)
	}
}