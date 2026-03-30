package http

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"io"
	"log"
	http "net/http"

	"github.com/gorilla/sessions"
	"github.com/jthomasw/YABA-2026/errs"
	"github.com/jthomasw/YABA-2026/foo"
	"golang.org/x/crypto/bcrypt"
)

func NewServer(attachments ServerAttachments) http.Server {
	router := http.NewServeMux()
	router.HandleFunc("POST /foo/v1", authMiddleware(newHandleFooV1Post(attachments.FooService), attachments.Store))
	router.HandleFunc("GET /foo/v1/{id}", authMiddleware(newHandleFooV1Get(attachments.FooService), attachments.Store))
	router.HandleFunc("/", loginPage)
	router.HandleFunc("/register", registerPage)
	router.HandleFunc("POST /login", loginUser(attachments.DB, attachments.Store))
	router.HandleFunc("POST /register-user", registerUser(attachments.DB, attachments.Store))
	router.HandleFunc("/dashboard", authMiddleware(dashboard(attachments.Store), attachments.Store))
	router.HandleFunc("/logout", logout(attachments.Store))
	router.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	server := http.Server{
		Handler: router,
		Addr:    ":8080",
	}
	return server
}

type ServerAttachments struct {
	FooService *foo.Service
	DB         *sql.DB
	Store      *sessions.CookieStore
}

func authMiddleware(next http.HandlerFunc, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		if session.Values["user"] == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func newHandleFooV1Post(fooService *foo.Service) http.HandlerFunc {
	return func(httpResponseWriter http.ResponseWriter, httpRequest *http.Request) {
		ctx := httpRequest.Context()
		httpRequestBody, err := io.ReadAll(httpRequest.Body)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusBadRequest)
			return
		}
		var fooRequest foo.CreateBarRequest
		err = json.Unmarshal(httpRequestBody, &fooRequest)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusBadRequest)
			return
		}
		fooResponse, err := fooService.CreateBar(ctx, fooRequest)
		if err != nil {
			switch errs.Cause(err).(type) {
			case errs.BadRequest:
				httpResponseWriter.WriteHeader(http.StatusBadRequest)
				return
			default:
				httpResponseWriter.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		httpResponseBody, err := json.Marshal(fooResponse)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}
		httpResponseWriter.Header().Add("Content-Type", "application/json")
		_, err = httpResponseWriter.Write(httpResponseBody)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}
		// httpResponseWriter.WriteHeader(http.StatusOK) Shown as example, but not needed if there is a successful write to the body.
		// Will actually generate a log message saying a superfluous header write was made or something like that
	}
}

func newHandleFooV1Get(fooService *foo.Service) http.HandlerFunc {
	return func(httpResponseWriter http.ResponseWriter, httpRequest *http.Request) {
		ctx := httpRequest.Context()
		id := httpRequest.PathValue("id") // The path value here needs to match what was inside the curly braces above, {id}, so "id" here
		fooRequest := foo.GetBarByIdRequest{
			Id: id,
		}
		bar, err := fooService.GetBarById(ctx, fooRequest)
		if err != nil {
			switch errs.Cause(err).(type) {
			case errs.BadRequest:
				httpResponseWriter.WriteHeader(http.StatusBadRequest)
				return
			default:
				httpResponseWriter.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		httpResponseBody, err := json.Marshal(bar)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}
		httpResponseWriter.Header().Add("Content-Type", "application/json")
		_, err = httpResponseWriter.Write(httpResponseBody)
		if err != nil {
			httpResponseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
}

func registerPage(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("templates/register.html"))
	if err := tmpl.Execute(w, nil); err != nil {
		log.Println("Error rendering register page:", err)
		http.Error(w, "Template rendering error", http.StatusInternalServerError)
	}
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("templates/login.html"))
	if err := tmpl.Execute(w, nil); err != nil {
		log.Println("Error rendering login page:", err)
		http.Error(w, "Template rendering error", http.StatusInternalServerError)
	}
}

func registerUser(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
}

func loginUser(db *sql.DB, store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
}

func dashboard(store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		user := session.Values["user"]

		if user == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		tmpl := template.Must(template.ParseFiles("templates/dashboard.html"))
		if err := tmpl.Execute(w, user); err != nil {
			log.Println("Error rendering dashboard:", err)
			http.Error(w, "Template rendering error", http.StatusInternalServerError)
		}
	}
}

func logout(store *sessions.CookieStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		session.Options.MaxAge = -1
		session.Save(r, w)

		log.Println("User logged out")
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
