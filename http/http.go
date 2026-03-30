package http

import (
	"html/template"
	"log"
	gohttp "net/http"

	"myapp/errs"
	"myapp/user"

	"github.com/gorilla/sessions"
)

const sessionName = "session"
const sessionUserKey = "user"

// Server wraps the standard http.Server so main can call ListenAndServe / Shutdown.
type Server struct {
	gohttp.Server
}

// ServerAttachments carries the dependencies injected into NewServer.
type ServerAttachments struct {
	UserService  *user.Service
	SessionStore *sessions.CookieStore
}

func NewServer(attachments ServerAttachments) Server {
	mux := gohttp.NewServeMux()

	// Static files
	mux.Handle("/static/",
		gohttp.StripPrefix("/static/",
			gohttp.FileServer(gohttp.Dir("static"))))

	// Routes
	mux.HandleFunc("/", newHandleLoginPage())
	mux.HandleFunc("/register", newHandleRegisterPage())
	mux.HandleFunc("/login", newHandleLogin(attachments.UserService, attachments.SessionStore))
	mux.HandleFunc("/register-user", newHandleRegisterUser(attachments.UserService))
	mux.HandleFunc("/dashboard", newHandleDashboard(attachments.SessionStore))
	mux.HandleFunc("/logout", newHandleLogout(attachments.SessionStore))

	return Server{
		gohttp.Server{
			Addr:    ":8080",
			Handler: mux,
		},
	}
}

// --- Page handlers ---

func newHandleLoginPage() gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		tmpl := template.Must(template.ParseFiles("templates/login.html"))
		tmpl.Execute(w, nil)
	}
}

func newHandleRegisterPage() gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		tmpl := template.Must(template.ParseFiles("templates/register.html"))
		tmpl.Execute(w, nil)
	}
}

func newHandleDashboard(store *sessions.CookieStore) gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		session, _ := store.Get(r, sessionName)
		username := session.Values[sessionUserKey]
		if username == nil {
			gohttp.Redirect(w, r, "/", gohttp.StatusSeeOther)
			return
		}
		tmpl := template.Must(template.ParseFiles("templates/dashboard.html"))
		tmpl.Execute(w, username)
	}
}

// --- Action handlers ---

func newHandleRegisterUser(userService *user.Service) gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		if r.Method != gohttp.MethodPost {
			gohttp.Redirect(w, r, "/register", gohttp.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			gohttp.Error(w, "form parsing error", gohttp.StatusBadRequest)
			return
		}

		req := user.RegisterRequest{
			Username: r.FormValue("username"),
			Password: r.FormValue("password"),
		}

		err := userService.Register(r.Context(), req)
		if err != nil {
			switch errs.Cause(err).(type) {
			case errs.BadRequest:
				gohttp.Error(w, err.Error(), gohttp.StatusBadRequest)
			default:
				gohttp.Error(w, "internal server error", gohttp.StatusInternalServerError)
			}
			return
		}

		log.Println("User registered:", req.Username)
		gohttp.Redirect(w, r, "/", gohttp.StatusSeeOther)
	}
}

func newHandleLogin(userService *user.Service, store *sessions.CookieStore) gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		if r.Method != gohttp.MethodPost {
			gohttp.Redirect(w, r, "/", gohttp.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			gohttp.Error(w, "form parsing error", gohttp.StatusBadRequest)
			return
		}

		req := user.LoginRequest{
			Username: r.FormValue("username"),
			Password: r.FormValue("password"),
		}

		username, err := userService.Authenticate(r.Context(), req)
		if err != nil {
			switch errs.Cause(err).(type) {
			case errs.Unauthorized:
				gohttp.Error(w, "invalid username or password", gohttp.StatusUnauthorized)
			case errs.BadRequest:
				gohttp.Error(w, err.Error(), gohttp.StatusBadRequest)
			default:
				gohttp.Error(w, "internal server error", gohttp.StatusInternalServerError)
			}
			return
		}

		session, _ := store.Get(r, sessionName)
		session.Values[sessionUserKey] = username
		session.Save(r, w)

		log.Println("User logged in:", username)
		gohttp.Redirect(w, r, "/dashboard", gohttp.StatusSeeOther)
	}
}

func newHandleLogout(store *sessions.CookieStore) gohttp.HandlerFunc {
	return func(w gohttp.ResponseWriter, r *gohttp.Request) {
		session, _ := store.Get(r, sessionName)
		session.Options.MaxAge = -1
		session.Save(r, w)

		log.Println("User logged out")
		gohttp.Redirect(w, r, "/", gohttp.StatusSeeOther)
	}
}
