// Package web holds the HTTP layer: routing, middleware, templates and
// request handling. It talks to the database only through *store.Store.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/sessions"

	"github.com/jthomasw/YABA-2026/internal/mail"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// Templates and static assets are compiled into the binary, parsed once at
// startup, so the built binary runs from any directory and a template error
// fails the start rather than a request.
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

	// ReceiptReading says receipts are read automatically (a Gemini key is
	// configured). When false the upload page says so before anything is
	// uploaded, instead of every receipt coming back "could not be read".
	ReceiptReading bool

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

	// Two keys, not one: with a single key gorilla signs the cookie but leaves
	// it readable. The cookie holds only a session id, but that id is a bearer
	// token for the row it names, and it should not be sitting in plain sight
	// in proxy logs, browser profiles and crash dumps. The second key makes the
	// cookie authenticated *and* encrypted.
	//
	// It is derived from the configured secret rather than asked for as another
	// environment variable, so no deployment has to learn a new setting. The
	// one visible cost is that cookies issued before this change no longer
	// decode, which signs everybody out once.
	cookies := sessions.NewCookieStore(cfg.SessionKey, deriveKey(cfg.SessionKey, "cookie-encryption-v1"))
	cookies.Options = &sessions.Options{
		Path: "/",
		// The same lifetime as the server-side session row. The row is what
		// actually decides whether a login is valid (including the idle
		// timeout); a shorter cookie only signed people out early.
		MaxAge:   int(store.SessionTTL.Seconds()),
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
	mux.HandleFunc("GET  /register", s.handleRegister)
	mux.HandleFunc("POST /register", s.handleRegisterSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.Handle("GET  /setup", s.authed(s.handleSetup))
	mux.Handle("POST /setup", s.authed(s.handleSetupSubmit))
	// Liveness and readiness. Public and uninformative on purpose: see
	// handleHealth.
	mux.HandleFunc("GET  /healthz", s.handleHealth)

	mux.HandleFunc("GET  /about", s.handleAbout)   // the `Learn more!` link
	mux.HandleFunc("GET  /forgot", s.handleForgot) // the `Forgot password` link

	// Password reset. All four are public by necessity: someone who cannot sign in is
	// exactly who needs them.
	mux.HandleFunc("POST /forgot", s.handleForgotRequest)
	mux.HandleFunc("GET  /reset", s.handleResetForm)
	mux.HandleFunc("POST /reset", s.handleResetSubmit)

	mux.Handle("GET  /dashboard", s.authed(s.handleDashboard))
	mux.Handle("GET  /notifications", s.authed(s.handleNotifications))

	// The old Savings page is gone: savings funds are on the Current Funds tab
	// (the emergency fund has its own tab). The old path redirects rather than
	// 404ing any bookmark that still points at it.
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
	mux.Handle("POST /income/{id}/edit", s.canEdit(s.handleRecurringIncomeUpdate))
	mux.Handle("POST /income/{id}/cancel", s.canEdit(s.handleRecurringIncomeCancel))
	mux.Handle("GET  /expense", s.canEdit(s.handleExpensePage))
	mux.Handle("POST /expense", s.canEdit(s.handleExpenseCreate))
	mux.Handle("POST /expense/recurring/{id}/edit", s.canEdit(s.handleRecurringExpenseUpdate))
	mux.Handle("POST /expense/recurring/{id}/cancel", s.canEdit(s.handleRecurringExpenseCancel))

	mux.Handle("GET  /transactions", s.authed(s.handleTransactions))
	mux.Handle("GET  /transactions/export.csv", s.authed(s.handleExportCSV))
	mux.Handle("GET  /transactions/new", s.canEdit(s.handleTransactionForm))
	mux.Handle("POST /transactions/new", s.canEdit(s.handleTransactionCreate))
	mux.Handle("GET  /transactions/{id}/edit", s.canEdit(s.handleTransactionForm))
	mux.Handle("POST /transactions/{id}/edit", s.canEdit(s.handleTransactionUpdate))
	mux.Handle("POST /transactions/{id}/delete", s.canEdit(s.handleTransactionDelete))
	mux.Handle("GET  /transactions/{id}/receipt", s.authed(s.handleReceipt))
	// The image of a receipt that is not yet an expense, for the confirmation
	// form to show beside the figures OCR read off it. Only authed, like the
	// route above: looking at a receipt is reading, and a viewer may read.
	mux.Handle("GET  /receipts/{id}/image", s.authed(s.handleReceiptPreview))

	// Receipts uploaded but not yet entered. This list used to sit at the top of
	// Add Expense; it is a page of its own now, so uploading offers two choices
	// and nothing else. Reading, so any member may look.
	mux.Handle("GET  /receipts", s.authed(s.handleReceiptsPage))
	// Polled by the upload page while the worker reads a receipt, so the page can
	// show progress rather than sending the user away to wait for a notification.
	mux.Handle("GET  /receipts/{id}/status", s.authed(s.handleReceiptStatus))

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
	mux.Handle("POST /household/rename", s.canManage(s.onShownHousehold(s.handleHouseholdRename)))
	mux.Handle("POST /household/delete", s.canManage(s.onShownHousehold(s.handleHouseholdDelete)))
	mux.Handle("POST /household/leave", s.authed(s.onShownHousehold(s.handleHouseholdLeave)))
	mux.Handle("POST /household/invite", s.canManage(s.onShownHousehold(s.handleInviteCreate)))
	mux.Handle("POST /household/invites/{id}/revoke", s.canManage(s.onShownHousehold(s.handleInviteRevoke)))
	// Resending is its own action rather than revoke-then-invite: with a 24-hour
	// expiry it will be needed often, and two steps that can half-fail is a poor
	// way to do one thing.
	mux.Handle("POST /household/invites/{id}/resend", s.canManage(s.onShownHousehold(s.handleInviteResend)))
	mux.Handle("POST /household/members/{id}/role", s.canManage(s.onShownHousehold(s.handleMemberRole)))
	mux.Handle("POST /household/members/{id}/remove", s.canManage(s.onShownHousehold(s.handleMemberRemove)))
	// Handing over the budget. One action, one transaction: promoting then
	// demoting by hand leaves the household briefly co-owned, and the reverse
	// order is blocked outright by the last-owner rule.
	mux.Handle("POST /household/members/{id}/transfer", s.canManage(s.onShownHousehold(s.handleTransferOwnership)))

	// Answering an invitation is not scoped to the household being joined -- the user is
	// not a member of it yet, so canManage would refuse.
	// The link in an invitation email. Not behind authed: the secret is kept
	// in this browser first, so it still counts after signing in.
	mux.HandleFunc("GET  /invites/open", s.handleInviteOpen)
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

	return recoverPanics(logRequests(s.securityHeaders(s.limitBodies(mux))))
}

// uploadRoutes are the only routes that accept a file, so the only ones whose
// body may be as large as a receipt. Everything else is a small form.
var uploadRoutes = map[string]bool{
	"/import/receipt":   true,
	"/transactions/new": true,
	"/expense":          true,
}

// formBodyLimit bounds every other request body. A form here is a few hundred
// bytes; a megabyte is generous and keeps an unauthenticated POST to /auth or
// /forgot from making the server read (or spool to disk) megabytes.
const formBodyLimit = 1 << 20

// bodyLimit is the most a request to this path may send.
func (s *Server) bodyLimit(path string) int64 {
	if uploadRoutes[path] {
		return s.maxUploadBytes() + bodySlack
	}
	return formBodyLimit
}

// limitBodies bounds the body of every unsafe request, public routes included,
// before anything parses it: Go's multipart parser would otherwise spool an
// arbitrarily large body to the temp directory.
func (s *Server) limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit(r.URL.Path))
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the headers every response should carry -- error pages,
// JSON and static files included, not only rendered pages.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		// Only when the deployment is HTTPS, which is what a Secure cookie
		// means here: HSTS tells browsers never to try plain HTTP for a year,
		// which would lock out a local http://localhost setup.
		if s.cfg.SecureCookie {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts the HTTP server with sane timeouts and serves until ctx
// is cancelled, then stops accepting and lets in-flight requests finish.
//
// Before this took a context there was no shutdown path at all: SIGTERM -- which
// is every deploy, every restart, every container eviction -- killed the process
// where it stood. A POST that was halfway through saving money was cut off
// mid-response, and the user got a connection reset with no way to tell whether
// their money had been recorded. Deferred cleanup (closing the database,
// stopping the worker) never ran either.
//
// The grace period is deliberately shorter than WriteTimeout, so a request that
// has already hung is not what keeps a deploy waiting.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: s.Handler(),
		// Without timeouts a client that never finishes its request holds a
		// goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Explicit rather than inherited, so the bound is visible here with the
		// others rather than being a default somebody has to know about.
		MaxHeaderBytes: 1 << 20,
	}

	errs := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		// Failed to bind, or stopped on its own.
		return err
	case <-ctx.Done():
	}

	log.Printf("shutdown: signal received; no longer accepting connections, " +
		"finishing in-flight requests")

	grace, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(grace); err != nil {
		// Something was still running when the grace period ran out. Say so
		// rather than returning silently: it is the one thing an operator
		// looking at a truncated request needs to know.
		log.Printf("shutdown: grace period expired with requests still running: %v", err)
		return err
	}

	log.Printf("shutdown: all requests finished")
	return <-errs
}
