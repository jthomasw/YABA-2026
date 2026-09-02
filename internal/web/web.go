// Package web holds the HTTP layer: routing, middleware, templates and
// request handling. It talks to the database only through *store.Store.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gorilla/sessions"

	"github.com/jthomasw/YABA-2026/internal/mail"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// Templates and static assets are compiled into the binary.
//
// The old code called template.ParseFiles("templates/dashboard.html", ...)
// inside the handler on every single request. That re-read five files from
// disk per page load, and because it used template.Must, a typo in any
// template panicked the whole process instead of returning a 500. It also
// meant the program only worked when launched from the repository root:
// running the built .exe from anywhere else produced a blank page.
//
//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Waker is the part of the background worker the web layer needs.
type Waker interface {
	// Wake asks the worker to check its queue now. Must not block.
	Wake()
}

// Config carries everything the server needs from its environment.
type Config struct {
	Addr         string
	SessionKey   []byte
	UploadDir    string
	SecureCookie bool
	MaxUploadMB  int64

	// Worker is nudged after a receipt upload so it is picked up immediately rather than
	// on the next poll.
	Worker Waker

	// Mail sends invitations and password reset links.
	Mail *mail.Mailer
}

// Server is the application's HTTP handler set.
type Server struct {
	store     *store.Store
	sessions  *sessions.CookieStore
	templates map[string]*template.Template
	cfg       Config
	mail      *mail.Mailer

	// staticFS is the embedded asset tree with the "static/" prefix stripped.
	staticFS fs.FS
	// assets maps each asset to a hash of its contents, for cache busting.
	assets assetFingerprints
}

// wakeWorker nudges the background worker if one was supplied.
func (s *Server) wakeWorker() {
	if s.cfg.Worker != nil {
		s.cfg.Worker.Wake()
	}
}

// New builds a Server and parses every template once.
func New(st *store.Store, cfg Config) (*Server, error) {
	// Sub() strips the leading "static/" so URLs stay at /static/style.css.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("embedded static FS is malformed: %w", err)
	}

	assets, err := buildFingerprints(sub)
	if err != nil {
		return nil, err
	}

	tmpl, err := parseTemplates(assets)
	if err != nil {
		return nil, err
	}

	cookies := sessions.NewCookieStore(cfg.SessionKey)
	cookies.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		// Secure is driven by config rather than hardcoded false, so a deployment behind TLS
		// gets a cookie the browser will not send over plain HTTP.
		Secure: cfg.SecureCookie,
		// Lax blocks the cookie on cross-site POSTs. A second line of defence behind the
		// CSRF token, not a replacement: not every browser enforces SameSite alike.
		SameSite: http.SameSiteLaxMode,
	}

	// A nil mailer would panic on the first invitation, and the flows are
	// supposed to work without SMTP configured, so build the log-only one here
	// rather than making every call site check.
	mailer := cfg.Mail
	if mailer == nil {
		mailer = mail.New(mail.Config{})
	}

	return &Server{
		store:     st,
		sessions:  cookies,
		templates: tmpl,
		cfg:       cfg,
		mail:      mailer,
		staticFS:  sub,
		assets:    assets,
	}, nil
}

// Handler returns the fully wired router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22+ patterns include the method, so a GET can no longer reach a handler that
	// mutates data.
	mux.HandleFunc("GET  /{$}", s.handleLanding)
	mux.HandleFunc("POST /auth", s.handleAuth)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET  /about", s.handleAbout)   // the `Learn more!` link
	mux.HandleFunc("GET  /forgot", s.handleForgot) // the `Forgot password` link

	// Password reset. All four are public by necessity: someone who cannot sign in is
	// exactly who needs them.
	mux.HandleFunc("POST /forgot", s.handleForgotRequest)
	mux.HandleFunc("GET  /reset", s.handleResetForm)
	mux.HandleFunc("POST /reset", s.handleResetSubmit)

	mux.Handle("GET  /dashboard", s.authed(s.handleDashboard))
	mux.Handle("GET  /notifications", s.authed(s.handleNotifications))

	// Savings moved onto the Emergency Fund tab. The old path redirects rather
	// than 404ing any bookmark that still points at it.
	mux.Handle("GET  /savings", s.authed(s.handleSavingsRedirect))
	mux.Handle("GET  /reports", s.authed(s.handleReports))

	// Permissions. The wrapper says who may reach a route, visible in the routing table
	// rather than buried in each handler: authed is any member and reads only, canEdit is
	// owners and editors, canMoveFunds and canManage are owners alone.

	// Recurring monthly expense buckets, priority ordered.
	mux.Handle("POST /buckets", s.canEdit(s.handleBucketCreate))
	mux.Handle("POST /buckets/{id}/edit", s.canEdit(s.handleBucketUpdate))
	mux.Handle("POST /buckets/{id}/up", s.canEdit(s.handleBucketUp))
	mux.Handle("POST /buckets/{id}/down", s.canEdit(s.handleBucketDown))
	mux.Handle("POST /buckets/{id}/archive", s.canEdit(s.handleBucketArchive))
	mux.Handle("POST /buckets/reallocate", s.canEdit(s.handleReallocate))

	// Two dedicated entry pages, so nothing on either is ambiguous about which direction
	// the money goes.
	mux.Handle("GET  /income", s.canEdit(s.handleIncomePage))
	mux.Handle("POST /income", s.canEdit(s.handleIncomeCreate))
	mux.Handle("GET  /expense", s.canEdit(s.handleExpensePage))
	mux.Handle("POST /expense", s.canEdit(s.handleExpenseCreate))

	mux.Handle("GET  /transactions", s.authed(s.handleTransactions))
	mux.Handle("GET  /transactions/export.csv", s.authed(s.handleExportCSV))
	mux.Handle("GET  /transactions/new", s.canEdit(s.handleTransactionForm))
	mux.Handle("POST /transactions/new", s.canEdit(s.handleTransactionCreate))
	mux.Handle("GET  /transactions/{id}/edit", s.canEdit(s.handleTransactionForm))
	mux.Handle("POST /transactions/{id}/edit", s.canEdit(s.handleTransactionUpdate))
	mux.Handle("POST /transactions/{id}/delete", s.canEdit(s.handleTransactionDelete))
	mux.Handle("GET  /transactions/{id}/receipt", s.authed(s.handleReceipt))

	// The import chooser now lives on /expense, so the old path redirects there
	// rather than 404ing any bookmark or link that still points at it.
	mux.Handle("GET  /import", s.authed(s.handleImportRedirect))
	mux.Handle("POST /import/receipt", s.canEdit(s.handleReceiptUpload))
	// Throwing away an upload is a write to the household's receipts, so it is
	// canEdit like the upload itself -- and, unlike signing out a device, it is
	// not a personal action a viewer should be able to take.
	mux.Handle("POST /receipts/{id}/discard", s.canEdit(s.handleReceiptDiscard))

	// Creating a fund and setting its goal are planning, not moving money, so
	// an editor may do both. Deposits, withdrawals and closing are the owner's.
	mux.Handle("POST /funds", s.canEdit(s.handleFundCreate))
	mux.Handle("POST /funds/{id}/goal", s.canEdit(s.handleFundGoal))
	mux.Handle("POST /funds/{id}/deposit", s.canMoveFunds(s.handleFundDeposit))
	mux.Handle("POST /funds/{id}/withdraw", s.canMoveFunds(s.handleFundWithdraw))
	mux.Handle("POST /funds/{id}/close", s.canMoveFunds(s.handleFundClose))

	mux.Handle("POST /budgets", s.canEdit(s.handleBudgetSet))
	mux.Handle("POST /budgets/{id}/delete", s.canEdit(s.handleBudgetDelete))

	// The settings page is readable by any member -- seeing who else can view your
	// finances is not a privilege -- but every action on it is the owner's.
	mux.Handle("GET  /household", s.authed(s.handleHousehold))
	mux.Handle("POST /household", s.authed(s.handleHouseholdCreate))
	mux.Handle("POST /household/switch", s.authed(s.handleHouseholdSwitch))
	mux.Handle("POST /household/rename", s.canManage(s.handleHouseholdRename))
	mux.Handle("POST /household/delete", s.canManage(s.handleHouseholdDelete))
	mux.Handle("POST /household/leave", s.authed(s.handleHouseholdLeave))
	mux.Handle("POST /household/invite", s.canManage(s.handleInviteCreate))
	mux.Handle("POST /household/invites/{id}/revoke", s.canManage(s.handleInviteRevoke))
	// Resending is its own action rather than revoke-then-invite: with a 24-hour
	// expiry it will be needed often, and two steps that can half-fail is a poor
	// way to do one thing.
	mux.Handle("POST /household/invites/{id}/resend", s.canManage(s.handleInviteResend))
	mux.Handle("POST /household/members/{id}/role", s.canManage(s.handleMemberRole))
	mux.Handle("POST /household/members/{id}/remove", s.canManage(s.handleMemberRemove))
	// Handing over the budget. One action, one transaction: promoting then
	// demoting by hand leaves the household briefly co-owned, and the reverse
	// order is blocked outright by the last-owner rule.
	mux.Handle("POST /household/members/{id}/transfer", s.canManage(s.handleTransferOwnership))

	// Answering an invitation is not scoped to the household being joined -- the user is
	// not a member of it yet, so canManage would refuse.
	mux.Handle("POST /invites/{id}/accept", s.authed(s.handleInviteAccept))
	mux.Handle("POST /invites/{id}/decline", s.authed(s.handleInviteDecline))

	// Only authed, deliberately: these act on the caller's own logins rather than on a
	// household's money, so a viewer must be able to sign out their own laptop.
	mux.Handle("GET  /sessions", s.authed(s.handleSessions))
	mux.Handle("POST /sessions/revoke", s.authed(s.handleSessionRevoke))
	mux.Handle("POST /sessions/revoke-others", s.authed(s.handleSessionRevokeOthers))

	// Changing your own password without a terminal.
	mux.Handle("GET  /password", s.authed(s.handlePasswordForm))
	mux.Handle("POST /password", s.authed(s.handlePasswordChange))

	// Static assets, served with a content-hashed URL and a long cache lifetime.
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(s.staticFS)))

	// Note what is absent: no file server rooted at ./uploads.

	return recoverPanics(logRequests(mux))
}

// ListenAndServe starts the HTTP server with sane timeouts.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: s.Handler(),
		// The old server set no timeouts at all, so a client that opened a
		// connection and never finished its request held a goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}

// ═════════════════════════════════════════════════════════════════════════════
// middleware.go
// ═════════════════════════════════════════════════════════════════════════════

const sessionName = "yaba_session"

// bodySlack is headroom over the upload limit for multipart boundaries, field
// names and the other form values that travel alongside the file.
const bodySlack = 1 << 20

// Session keys. The cookie carries a session id naming a row in the sessions table,
// not a user id: an id in a signed cookie can be verified but never cancelled,
// whereas a row can be deleted.
const (
	sessionID        = "sid"
	sessionCSRFToken = "csrf"
	sessionFlashText = "flash_text"
	sessionFlashKind = "flash_kind"
)

// userCtxKey is unexported so no other package can plant a user in the context.
type userCtxKey struct{}

// sessionCtxKey carries the current session's token, so the device list can mark
// which row is the one you are reading it from.
type sessionCtxKey struct{}

// currentSession returns the token of the session making this request.
func currentSession(r *http.Request) string {
	id, _ := r.Context().Value(sessionCtxKey{}).(string)
	return id
}

// membershipCtxKey carries the household the request is operating on, together
// with the caller's role in it. Unexported for the same reason.
type membershipCtxKey struct{}

// userFrom retrieves the authenticated user placed by the authed middleware.
func userFrom(r *http.Request) (store.User, bool) {
	u, ok := r.Context().Value(userCtxKey{}).(store.User)
	return u, ok
}

// mustUser returns the authenticated user. Handlers behind authed can rely on
// it; reaching the panic would mean a route was registered without the
// middleware, which is a programming error worth failing loudly on.
func mustUser(r *http.Request) store.User {
	u, ok := userFrom(r)
	if !ok {
		panic("handler requires authentication but was not wrapped in authed")
	}
	return u
}

// mustMembership returns the household the request is working in and the caller's role.
func mustMembership(r *http.Request) store.Membership {
	m, ok := r.Context().Value(membershipCtxKey{}).(store.Membership)
	if !ok {
		panic("handler requires a household but was not wrapped in authed")
	}
	return m
}

// scopeOf builds the store.Scope for this request: which household owns the data, and
// who is acting.
func scopeOf(r *http.Request) store.Scope {
	return store.Scope{
		HouseholdID: mustMembership(r).ID,
		UserID:      mustUser(r).ID,
	}
}

// authed wraps a handler with authentication and, for unsafe methods, CSRF
// verification. Both live in one place, so adding a route cannot omit either.
func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := s.sessions.Get(r, sessionName)
		if err != nil {
			// A cookie signed with a rotated key fails to decode.
			s.clearSession(w, r)
			s.redirectToLogin(w, r)
			return
		}

		sid, ok := session.Values[sessionID].(string)
		if !ok || sid == "" {
			s.redirectToLogin(w, r)
			return
		}

		// The session is resolved against the database on every request rather than trusted
		// from the cookie.
		user, err := s.store.SessionUser(r.Context(), sid)
		if errors.Is(err, store.ErrNotFound) {
			s.clearSession(w, r)
			s.redirectToLogin(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		uid := user.ID

		// Bound the body before anything parses it: Go's multipart parser will spool an
		// arbitrarily large upload to the temp directory.
		if !isSafeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes()+bodySlack)
		}

		if !isSafeMethod(r.Method) && !s.checkCSRF(r, session) {
			// 403 rather than a redirect: a redirect would look like success to an attacking
			// page and like a mystery to a real user whose token expired.
			log.Printf("CSRF rejected: %s %s user=%d", r.Method, r.URL.Path, uid)
			http.Error(w, "This form has expired. Go back, reload the page, and try again.",
				http.StatusForbidden)
			return
		}

		// The household is resolved per request for the same reason: removal from a shared
		// budget takes effect immediately, because ActiveHousehold joins household_members.
		hh, err := s.store.ActiveHousehold(r.Context(), user)
		if err != nil {
			s.serverError(w, r, err)
			return
		}

		ctx := context.WithValue(r.Context(), userCtxKey{}, user)
		ctx = context.WithValue(ctx, membershipCtxKey{}, hh)
		ctx = context.WithValue(ctx, sessionCtxKey{}, sid)
		next(w, r.WithContext(ctx))
	})
}

// requirePerm gates a handler on the caller's role.
func (s *Server) requirePerm(
	allowed func(store.Role) bool,
	reason string,
	next http.HandlerFunc,
) http.Handler {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(mustMembership(r).Role) {
			// A viewer never sees the button that posts here, so arriving means a page left open
			// while somebody changed your role, or a deliberate probe.
			log.Printf("permission refused: %s %s user=%d role=%s",
				r.Method, r.URL.Path, mustUser(r).ID, mustMembership(r).Role)

			if isSafeMethod(r.Method) {
				s.renderForbidden(w, r, reason)
				return
			}
			s.flashError(w, r, reason)
			http.Redirect(w, r, backTo(r), http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

// canEdit permits owners and editors: recording income, expenses, recurring
// buckets and category budgets.
func (s *Server) canEdit(next http.HandlerFunc) http.Handler {
	return s.requirePerm(store.Role.CanEditEntries,
		"You have view-only access to this budget, so you cannot add or change entries.",
		next)
}

// canMoveFunds permits owners only: deposits, withdrawals and closing a savings fund.
func (s *Server) canMoveFunds(next http.HandlerFunc) http.Handler {
	return s.requirePerm(store.Role.CanMoveFunds,
		"Only the owner of this budget can move money into or out of savings.",
		next)
}

// canManage permits owners only: members, invitations, renaming and deletion.
func (s *Server) canManage(next http.HandlerFunc) http.Handler {
	return s.requirePerm(store.Role.CanManageMembers,
		"Only the owner of this budget can manage who has access.",
		next)
}

// backTo picks a safe place to return to after a refused POST: only the path of a same-
// origin Referer.
func backTo(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "/dashboard"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return "/dashboard"
	}
	// Drop scheme, host and userinfo: whatever is left cannot leave this site.
	back := u.Path
	if u.RawQuery != "" {
		back += "?" + u.RawQuery
	}
	return back
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── CSRF ──────────────────────────────────────────────────────────────────────

// csrfFormField is the hidden input name every form must include.
const csrfFormField = "csrf_token"

// csrfToken returns the session's CSRF token, minting one if needed.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		session, _ = s.sessions.New(r, sessionName)
	}

	if tok, ok := session.Values[sessionCSRFToken].(string); ok && tok != "" {
		return tok
	}

	tok, err := randomToken(32)
	if err != nil {
		// Without a token, forms cannot be submitted safely.
		log.Printf("csrf: could not generate token: %v", err)
		return ""
	}
	session.Values[sessionCSRFToken] = tok
	if err := session.Save(r, w); err != nil {
		log.Printf("csrf: could not save session: %v", err)
	}
	return tok
}

// checkCSRF compares the submitted token with the session's.
func (s *Server) checkCSRF(r *http.Request, session *sessions.Session) bool {
	want, ok := session.Values[sessionCSRFToken].(string)
	if !ok || want == "" {
		return false
	}

	// r.FormValue would consume a multipart body before the handler can read the file, so
	// the token is read from the URL-encoded values only after ParseMultipartForm has run.
	got := r.PostFormValue(csrfFormField)
	if got == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(s.maxUploadBytes()); err == nil {
			got = r.PostFormValue(csrfFormField)
		}
	}
	if got == "" {
		return false
	}

	// Constant-time comparison: a plain == leaks how many leading bytes
	// matched through its timing, which is enough to guess a token byte by byte.
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ── flash messages ────────────────────────────────────────────────────────────

// flash is a one-shot message shown after a redirect, so a rejected action and a
// successful one no longer produce identical screens.
type flash struct {
	Kind string // "success" | "error"
	Text string
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, text string) {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		session, _ = s.sessions.New(r, sessionName)
	}
	session.Values[sessionFlashKind] = kind
	session.Values[sessionFlashText] = text
	if err := session.Save(r, w); err != nil {
		log.Printf("flash: save: %v", err)
	}
}

func (s *Server) flashSuccess(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, "success", text)
}

func (s *Server) flashError(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, "error", text)
}

// takeFlash reads and clears the pending message.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		return nil
	}
	text, _ := session.Values[sessionFlashText].(string)
	if text == "" {
		return nil
	}
	kind, _ := session.Values[sessionFlashKind].(string)

	delete(session.Values, sessionFlashText)
	delete(session.Values, sessionFlashKind)
	if err := session.Save(r, w); err != nil {
		log.Printf("flash: clear: %v", err)
	}
	return &flash{Kind: kind, Text: text}
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	session, _ := s.sessions.New(r, sessionName)
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		log.Printf("session: clear: %v", err)
	}
}

// ── login rate limiting ───────────────────────────────────────────────────────

// Rate limiting lives in the store, keyed on the same strings as before.

// clientIP extracts the address a request really came from.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !trustedProxyIP(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	return host
}

// trustedProxyIP reports whether a forwarded header from this peer may be believed.
func trustedProxyIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ── generic middleware ────────────────────────────────────────────────────────

// recoverPanics converts a panic in any handler into a 500 instead of killing
// the process and dropping every other in-flight request.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "Something went wrong on our end.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// Query strings are omitted: they can carry a search term, and access
		// logs are the classic place sensitive input leaks into plain text.
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond))
	})
}

// ═════════════════════════════════════════════════════════════════════════════
// render.go
// ═════════════════════════════════════════════════════════════════════════════

// pages lists every top-level template. Each one defines a "content" block
// that layout.html renders inside the shared chrome.
var pages = []string{
	"landing.html", // login and signup in one, per the wireframe
	"about.html",   // the `Learn more!` link
	"forgot.html",  // the `Forgot password` link
	"dashboard.html",
	"reports.html",
	"transactions.html",
	"transaction_form.html",
	"income.html",    // dedicated Add Income page
	"expense.html",   // dedicated Add Expense page, opening on Manual or Upload
	"household.html", // members, roles and invitations for a shared budget
	"sessions.html",  // active logins, with revocation
	"reset.html",     // choose a new password from an emailed link
	"password.html",  // change your own password while signed in
	"forbidden.html", // a role was not permitted to do something
}

// funcs are the helpers available to every template.
var funcs = template.FuncMap{
	// money returns "$1,234.56", with the sign outside the symbol.
	"money": func(c money.Cents) string { return c.Display() },
	// amount returns "1234.56" for a number input's value attribute.
	"amount": func(c money.Cents) string { return c.Input() },
	// signed prefixes a plus so income and expenses read differently.
	"signed": func(c money.Cents) string {
		if c > 0 {
			return "+" + c.Display()
		}
		return c.Display()
	},
	// negative reports whether an amount should be styled as an outflow.
	"negative": func(c money.Cents) bool { return c < 0 },
	// pct rounds a percentage for a progress bar width.
	"pct": func(f float64) string { return fmt.Sprintf("%.1f", f) },
	// pctInt rounds a percentage for display text.
	"pctInt": func(f float64) string { return fmt.Sprintf("%.0f", f) },

	// monthName turns "2026-04" into "April 2026" for a selector label.
	"monthName": func(m string) string {
		t, err := time.Parse(store.MonthLayout, m)
		if err != nil {
			return m
		}
		return t.Format("January 2006")
	},
	// monthShort turns "2026-04" into "Apr" for a chart axis.
	"monthShort": func(m string) string {
		t, err := time.Parse(store.MonthLayout, m)
		if err != nil {
			return m
		}
		return t.Format("Jan")
	},
	// dateName turns "2026-04-18" into "18 Apr 2026".
	"dateName": func(d string) string {
		t, err := time.Parse(store.DateLayout, d)
		if err != nil {
			return d
		}
		return t.Format("2 Jan 2006")
	},

	// jsonAttr marshals a value for a data- attribute.
	"jsonAttr": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},

	// centsFloats converts amounts to dollar floats for a chart axis.
	"centsFloats": func(cs []money.Cents) []float64 {
		out := make([]float64, len(cs))
		for i, c := range cs {
			out[i] = c.Float()
		}
		return out
	},

	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
	"seq": func(from, to int) []int {
		if to < from {
			return nil
		}
		out := make([]int, 0, to-from+1)
		for i := from; i <= to; i++ {
			out = append(out, i)
		}
		return out
	},
	"title": strings.Title, //nolint:staticcheck // ASCII labels only

	// Replaced per-Server in parseTemplates with one that knows the real fingerprints.
	"asset": func(name string) string { return "/static/" + name },
}

// parseTemplates parses layout, partials and each page exactly once at startup.
func parseTemplates(assets assetFingerprints) (map[string]*template.Template, error) {
	// Copy the shared map and override "asset" with one bound to the real fingerprints, so
	// templates can write {{asset "style.css"}} and get a cache-busted URL.
	fm := make(template.FuncMap, len(funcs)+1)
	for k, v := range funcs {
		fm[k] = v
	}
	fm["asset"] = assets.url

	out := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t, err := template.New(page).Funcs(fm).ParseFS(
			templateFS,
			"templates/layout.html",
			"templates/partials/*.html",
			"templates/"+page,
		)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		out[page] = t
	}
	return out, nil
}

// view is the data every page receives. Embedding it in each page's own view
// model keeps the shared fields (user, CSRF token, flash message) in one place
// so a new page cannot forget the CSRF token.
type view struct {
	Title     string
	Username  string
	Nav       string
	CSRFToken string
	Flash     *flash
	Now       string

	// Household is the budget on screen and Role is what this user may do to it.
	Household string
	Role      store.Role
	Personal  bool

	// Households is every budget this user can switch to, for the nav picker.
	Households []store.Household

	// Invites are unanswered invitations addressed to this user.
	Invites []store.Invite
}

// Shared reports whether the current budget has more than one member, so the UI
// can stay out of the way for somebody using YABA alone.
func (v view) Shared() bool { return len(v.Households) > 1 || !v.Personal }

// render writes a page, buffering first: a template failure partway through would
// otherwise have already sent a 200 and half a document.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	s.renderStatus(w, r, http.StatusOK, page, data)
}

// renderStatus renders with a specific status code.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	t, ok := s.templates[page]
	if !ok {
		log.Printf("render: unknown page %q", page)
		s.serverError(w, r, fmt.Errorf("unknown page %q", page))
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
		s.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")

	// no-store on every rendered page. Forms embed a CSRF token tied to the session and
	// signing in rotates it, so a cached page fails the check with a baffling message.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	// Chart.js is loaded from a CDN, so script-src allows it; everything else is same-
	// origin.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"form-action 'self'; "+
			"frame-ancestors 'none'; "+
			"base-uri 'none'")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// baseView fills in the fields shared by every page.
func (s *Server) baseView(w http.ResponseWriter, r *http.Request, title, nav string) view {
	v := view{
		Title: title,
		Nav:   nav,
		Now:   store.Today(),
	}
	if u, ok := userFrom(r); ok {
		// Name(), not the raw email: the header should read "kushith" rather than
		// printing the user's full address on every page they might screen-share.
		v.Username = u.Name()

		if m, ok := r.Context().Value(membershipCtxKey{}).(store.Membership); ok {
			v.Household, v.Role, v.Personal = m.Name, m.Role, m.Personal
		}

		// Two extra indexed reads on every page: the switcher is in the nav, and an
		// unanswered invitation should be visible wherever the user happens to be.
		if hs, err := s.store.HouseholdsFor(r.Context(), u.ID); err != nil {
			log.Printf("baseView: households for user=%d: %v", u.ID, err)
		} else {
			v.Households = hs
		}
		if inv, err := s.store.InvitesFor(r.Context(), u.ID, u.Email); err != nil {
			log.Printf("baseView: invites for user=%d: %v", u.ID, err)
		} else {
			v.Invites = inv
		}
	}
	v.CSRFToken = s.csrfToken(w, r)
	v.Flash = s.takeFlash(w, r)
	return v
}

// forbiddenView backs the 403 page.
type forbiddenView struct {
	view
	Reason string
}

// renderForbidden shows a proper page for a refused GET.
func (s *Server) renderForbidden(w http.ResponseWriter, r *http.Request, reason string) {
	v := forbiddenView{view: s.baseView(w, r, "Not permitted", ""), Reason: reason}
	s.renderStatus(w, r, http.StatusForbidden, "forbidden.html", v)
}

// serverError logs the real error and shows the user a generic page, so raw SQL never
// reaches the screen and a failed write never looks like a successful one.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("ERROR %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "Something went wrong on our end. Please try again.",
		http.StatusInternalServerError)
}

// writeJSON encodes v as a JSON response, buffered like render so a marshalling
// failure cannot follow half a document.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// This endpoint is polled and its whole purpose is to return something new,
	// so a cached response would defeat it.
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// badRequest reports a client mistake without echoing input back into HTML.
func (s *Server) badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

// ═════════════════════════════════════════════════════════════════════════════
// assets.go
// ═════════════════════════════════════════════════════════════════════════════

// assetFingerprints maps a static filename to a short hash of its contents.
type assetFingerprints map[string]string

// buildFingerprints hashes every embedded static file once, at startup.
func buildFingerprints(fsys fs.FS) (assetFingerprints, error) {
	out := assetFingerprints{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read asset %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		// Eight hex characters is 32 bits: ample to notice a change, and short
		// enough to keep the URL readable in devtools.
		out[p] = hex.EncodeToString(sum[:])[:8]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// url returns the cache-busted path for one asset.
func (a assetFingerprints) url(name string) string {
	name = strings.TrimPrefix(name, "/")
	name = path.Clean(name)
	if v, ok := a[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// staticHandler serves the embedded assets with a long cache lifetime, which is safe
// because the URL carries the content hash: a changed file is a different URL.
func staticHandler(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Reached without a fingerprint -- a hand-typed URL, or a stale page still
			// referencing the old form.
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
