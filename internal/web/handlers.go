package web


import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/insight"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// landingView backs the landing page, which is both the login form and the
// signup form.
//
// The wireframe describes one form, not two: "When a user clicks login, if the
// email already exists, then try to login with given password. If the email
// address does not already exist, then the GUI should ask the user if they want
// to create a new account and if so, prompt the user to type their password in
// one more time to confirm it."
//
// So there is no separate /register page. Confirming is a second state of the
// same page, which is what Confirming below switches on.
type landingView struct {
	view

	Error string
	Email string

	// Confirming is true when the address was not recognised and the user is
	// being asked whether to create an account.
	Confirming bool

	// NewAccount is set by ?new=1 from the "Sign up" link. It only adds an
	// explanatory line: the form and the flow are identical either way, because
	// an unknown address is offered an account regardless of how you arrived.
	NewAccount bool
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.signedIn(r) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	v := landingView{view: s.baseView(w, r, "Welcome to YABA", "landing")}
	v.NewAccount = r.URL.Query().Get("new") == "1"
	s.render(w, r, "landing.html", v)
}

// handleAuth is the single entry point for both signing in and signing up.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	// The landing form is unauthenticated but still carries a token, minted when
	// the page was rendered. That stops a third-party page from silently signing
	// a victim into an account the attacker controls.
	session, err := s.sessions.Get(r, sessionName)
	if err == nil && !s.checkCSRF(r, session) {
		s.renderLanding(w, r, http.StatusForbidden, landingView{
			Email: r.PostFormValue("email"),
			Error: "That form expired. Please try again.",
		})
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")
	// The confirm step posts back with this set, so the server knows the user
	// has already been asked whether they want an account.
	creating := r.PostFormValue("create") == "yes"

	if email == "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Error: "Please enter your email address."})
		return
	}

	// The address is checked for shape here, before it is looked up, so a value
	// that is not an email address is rejected whether or not an account happens
	// to match it.
	//
	// This matters because migration 3 backfilled email from the old username
	// column, so a legacy row can hold a bare name like "kushith". Validating
	// only on the signup path -- as this did before -- meant such a row could
	// still be signed into with a value the app would refuse to create today.
	if msg := validateEmail(email); msg != "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email, Confirming: creating, Error: msg})
		return
	}

	if password == "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email, Confirming: creating,
			Error: "Please enter a password."})
		return
	}

	// Rate limit on address and IP together. Keying on IP alone would let one
	// attacker lock out a shared network; on the address alone, anyone could
	// lock a known user out of their own account.
	key := clientIP(r) + "|" + store.NormalizeEmail(email)
	retryIn, err := s.store.RateRetryIn(r.Context(), key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if retryIn > 0 {
		s.renderLanding(w, r, http.StatusTooManyRequests, landingView{
			Email: email,
			Error: "Too many failed attempts. Try again " + retryPhrase(retryIn) + "."})
		return
	}

	exists, err := s.store.EmailExists(r.Context(), email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if exists {
		if creating {
			// The address was created between the two steps, or the user came
			// back to an old confirm form. Either way, sign in instead.
			log.Printf("auth: confirm step for an address that now exists")
		}
		s.attemptLogin(w, r, email, password, key)
		return
	}

	// Unknown address. First submission asks; second submission creates.
	// The address itself was already validated above.
	if !creating {
		s.renderLanding(w, r, http.StatusOK, landingView{
			Email:      email,
			Confirming: true,
		})
		return
	}

	if msg := validatePassword(password); msg != "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email, Confirming: true, Error: msg})
		return
	}
	if password != confirm {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email, Confirming: true,
			Error: "The two passwords do not match."})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	userID, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, store.ErrEmailTaken) {
		// Lost a race with another signup for the same address.
		s.attemptLogin(w, r, email, password, key)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	log.Printf("auth: created account %d", userID)
	if err := s.startSession(w, r, userID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flashSuccess(w, r, "Welcome to YABA. Add some income to get started.")
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// attemptLogin verifies a password for a known address.
func (s *Server) attemptLogin(w http.ResponseWriter, r *http.Request, email, password, rateKey string) {
	user, hash, err := s.store.CredentialsFor(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		// Compare against a dummy hash anyway, so an unknown address takes about
		// as long as a known one. Without this the response time reveals which
		// addresses have accounts.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		s.store.RateFail(r.Context(), rateKey)
		s.renderLanding(w, r, http.StatusUnauthorized, landingView{
			Email: email, Error: "That email and password do not match."})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.store.RateFail(r.Context(), rateKey)
		s.renderLanding(w, r, http.StatusUnauthorized, landingView{
			Email: email, Error: "That email and password do not match."})
		return
	}

	s.store.RateReset(r.Context(), rateKey)
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	log.Printf("auth: login ok user=%d", user.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// startSession issues a fresh session for a newly authenticated user.
//
// A brand-new session rather than reusing the incoming cookie, so the session
// identifier rotates on privilege change and a token planted before login is
// useless after it.
//
// The session is a ROW now, and the cookie only carries its token. That is what
// makes a login revocable: deleting the row ends it on the next request. The
// cookie is still signed, so a token cannot be tampered into a different one.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	sid, err := s.store.CreateSession(r.Context(), userID, r.UserAgent())
	if err != nil {
		return err
	}

	// Housekeeping, on a path that is already doing a write and is not
	// latency-sensitive. Failure here is irrelevant to the login, because
	// SessionUser refuses an expired row regardless of whether it still exists.
	if n, err := s.store.PurgeExpiredSessions(r.Context()); err != nil {
		log.Printf("auth: purge expired sessions: %v", err)
	} else if n > 0 {
		log.Printf("auth: purged %d expired session(s)", n)
	}

	session, _ := s.sessions.New(r, sessionName)
	session.Values[sessionID] = sid
	if tok, err := randomToken(32); err == nil {
		session.Values[sessionCSRFToken] = tok
	} else {
		log.Printf("auth: could not mint CSRF token: %v", err)
	}
	if err := session.Save(r, w); err != nil {
		// The row exists but the browser never got its token, so nothing can
		// present it. Delete it rather than leaving an unreachable row behind.
		if delErr := s.store.DeleteSession(r.Context(), userID, sid); delErr != nil {
			log.Printf("auth: orphaned session %s: %v", sid[:8], delErr)
		}
		return fmt.Errorf("save session cookie: %w", err)
	}
	return nil
}

// handleLogout is POST-only, which is why the nav renders it as a small form
// rather than a link: a GET /logout can be fired by any third-party page or by
// a browser prefetching the link.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, err := s.sessions.Get(r, sessionName)
	if err == nil && !s.checkCSRF(r, session) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	// Delete the row, not just the cookie. Clearing the cookie alone would leave
	// a live session behind: anyone holding a copy of that token -- from a shared
	// machine, a backup, a proxy log -- could keep using it. Signing out has to
	// destroy the credential, not merely forget it locally.
	if err == nil {
		if sid, ok := session.Values[sessionID].(string); ok && sid != "" {
			if u, uerr := s.store.SessionUser(r.Context(), sid); uerr == nil {
				if derr := s.store.DeleteSession(r.Context(), u.ID, sid); derr != nil &&
					!errors.Is(derr, store.ErrNotFound) {
					log.Printf("logout: could not delete session: %v", derr)
				}
			}
		}
	}

	s.clearSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── active devices ────────────────────────────────────────────────────────────

// sessionsView backs /sessions.
type sessionsView struct {
	view

	Sessions []store.Session

	// Others is how many logins other than this one are active, so the page can
	// hide the "sign out everywhere else" button when there is nothing to do.
	Others int
}

// handleSessions lists the account's active logins.
//
// Worth showing rather than just supporting: a user who can see "Chrome on
// Windows, active 3 minutes ago" alongside a device they do not recognise is a
// user who can act on it. Revocation nobody can see is revocation nobody uses.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	current := currentSession(r)

	list, err := s.store.Sessions(r.Context(), user.ID, current)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	v := sessionsView{
		view:     s.baseView(w, r, "Active devices", "sessions"),
		Sessions: list,
	}
	for _, sess := range list {
		if !sess.Current {
			v.Others++
		}
	}
	s.render(w, r, "sessions.html", v)
}

// handleSessionRevoke signs out one device.
func (s *Server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}
	target := r.PostFormValue("session_id")

	// Revoking the session you are using is just a logout, and saying so is
	// kinder than silently ending the request with a redirect that fails auth.
	if target == currentSession(r) {
		s.flashError(w, r, "That is this device. Use Log out instead.")
		http.Redirect(w, r, "/sessions", http.StatusSeeOther)
		return
	}

	// DeleteSession carries the user id in its WHERE clause, so a guessed token
	// cannot sign somebody else out.
	switch err := s.store.DeleteSession(r.Context(), user.ID, target); {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That device is already signed out.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, "That device is signed out. It will be asked to log in on its next click.")
	}
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}

// handleSessionRevokeOthers signs out every device except this one.
func (s *Server) handleSessionRevokeOthers(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	n, err := s.store.DeleteOtherSessions(r.Context(), user.ID, currentSession(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n == 0 {
		s.flashSuccess(w, r, "This was already your only active device.")
	} else {
		s.flashSuccess(w, r, fmt.Sprintf(
			"Signed out %d other device%s. You are still logged in here.",
			n, map[bool]string{true: "", false: "s"}[n == 1]))
	}
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}

// handleAbout is the target of the wireframe's `Learn more!` link.
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "about.html", s.baseView(w, r, "What is YABA?", "about"))
}

// handleForgot backs the `Forgot password` link.
//
// It does not pretend to send an email. Delivering a reset link needs an SMTP
// service that this project has not been given, and a page that claims "check
// your inbox" while sending nothing is worse than one that explains the
// situation -- the user would wait for a message that never arrives.
func (s *Server) handleForgot(w http.ResponseWriter, r *http.Request) {
	// forgotView, not the bare view: the template reads .Sent and .MailEnabled,
	// and html/template treats a missing field as an execution error rather than
	// an empty value -- so rendering the wrong type here would 500 the page.
	s.render(w, r, "forgot.html", forgotView{
		view:        s.baseView(w, r, "Forgot password", "forgot"),
		MailEnabled: s.mail.Enabled(),
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// dummyHash is a valid bcrypt hash of a fixed value, used to equalise timing on
// unknown addresses. Computed once at init so the cost is not paid per request.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equalisation-placeholder"), bcrypt.DefaultCost)
	if err != nil {
		return []byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinv")
	}
	return h
}()

// signedIn reports whether this request carries a session that is still live.
//
// It resolves the token against the sessions table rather than trusting the
// cookie, because a revoked or expired session must not count as signed in --
// otherwise the landing page would send a stale cookie to /dashboard, which
// authed would bounce straight back to the login page.
// issueFormToken mints a one-time token for a form that creates something.
//
// A failure here is logged and swallowed rather than failing the page: the token
// prevents an accidental duplicate, it is not a security control, and refusing
// to show somebody the Add Expense form because a token could not be issued
// would be a worse outcome than the duplicate it guards against.
func (s *Server) issueFormToken(r *http.Request, purpose string) string {
	user, ok := userFrom(r)
	if !ok {
		return ""
	}
	tok, err := s.store.NewFormToken(r.Context(), user.ID, purpose)
	if err != nil {
		log.Printf("form token: could not issue one for %s: %v", purpose, err)
		return ""
	}
	return tok
}

// duplicateSubmit reports whether this submission has already been processed.
//
// Called after validation and before the insert. Before validation would burn
// the token on a typo, so correcting the amount would then be refused as a
// duplicate — which is the opposite of helpful.
//
// A request carrying no token at all is allowed through. Every form renders one,
// so this only covers a page rendered before the feature existed, and refusing
// those would break a form somebody has open right now for no safety gain: this
// guards against a double click, not against an attacker, who can simply not
// send the field.
func (s *Server) duplicateSubmit(r *http.Request, userID int64) bool {
	tok := strings.TrimSpace(r.PostFormValue("form_token"))
	if tok == "" {
		return false
	}
	used, err := s.store.ConsumeFormToken(r.Context(), userID, tok)
	if err != nil {
		log.Printf("form token: could not consume one: %v", err)
		return false
	}
	return !used
}

// retryPhrase turns a remaining lockout into words.
//
// Rounded UP to whole minutes, and never down: telling somebody to come back in
// 9 minutes when it is really 9 minutes 40 seconds earns them a second refusal
// and a second helping of annoyance. Rounding up means the stated time works.
//
// Minutes only, never seconds. A countdown to the second invites watching it,
// and the precision is false anyway -- the number is read once, on a page that
// is not going to refresh itself.
func retryPhrase(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	minutes := int((d + time.Minute - 1) / time.Minute) // ceiling
	switch {
	case minutes <= 0:
		return "in less than a minute"
	case minutes == 1:
		return "in 1 minute"
	default:
		return fmt.Sprintf("in %d minutes", minutes)
	}
}

func (s *Server) signedIn(r *http.Request) bool {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		return false
	}
	sid, ok := session.Values[sessionID].(string)
	if !ok || sid == "" {
		return false
	}
	_, err = s.store.SessionUser(r.Context(), sid)
	return err == nil
}

// renderLanding fills in the shared view fields and renders, with the status
// passed through so the headers are not committed before render sets them.
func (s *Server) renderLanding(w http.ResponseWriter, r *http.Request, status int, v landingView) {
	v.view = s.baseView(w, r, "Welcome to YABA", "landing")
	s.renderStatus(w, r, status, "landing.html", v)
}

// validateEmail checks that the address is plausibly an email address.
//
// Deliberately not an RFC 5322 grammar. A full parser is famously not worth
// writing, and over-strict rules reject real addresses -- which is worse than
// letting a typo through, because a typo is discovered at the next sign-in
// whereas a false rejection locks someone out with no recourse. So this catches
// the things that are definitely wrong and nothing more.
const emailExample = "you@example.com"

func validateEmail(e string) string {
	e = strings.TrimSpace(e)
	if e == "" {
		return "Please enter your email address."
	}
	// 254 is the maximum length of an address in an SMTP envelope.
	if len(e) > 254 {
		return "That email address is too long."
	}

	invalid := "Please enter a valid email address, like " + emailExample + "."

	// Exactly one @, with something on each side.
	if strings.Count(e, "@") != 1 {
		return invalid
	}
	at := strings.IndexByte(e, '@')
	local, domain := e[:at], e[at+1:]
	if local == "" || domain == "" {
		return invalid
	}

	// No whitespace or characters that need quoting in a real address.
	if strings.ContainsAny(e, " \t\r\n,;:\"<>()[]\\") {
		return invalid
	}

	// The domain needs at least one dot, and neither the domain nor any of its
	// labels may be empty -- so "a@b", "a@.com", "a@b." and "a@b..com" all fail.
	if !strings.Contains(domain, ".") {
		return "Please enter a valid email address — the part after the @ needs a dot, like " +
			emailExample + "."
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.Contains(domain, "..") {
		return invalid
	}

	// A real top-level domain is at least two letters, which rules out a
	// trailing single character such as "a@b.c".
	labels := strings.Split(domain, ".")
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return invalid
	}
	for _, r := range tld {
		if !isLetter(r) {
			return invalid
		}
	}

	// A hyphen may appear inside a label but not at either end of one.
	for _, label := range labels {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return invalid
		}
	}

	return ""
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// validatePassword sets a floor without being obstructive.
//
// bcrypt silently truncates at 72 bytes, so anything longer is rejected rather
// than accepted and quietly shortened: otherwise two different long passwords
// sharing a 72-byte prefix would both open the account.
func validatePassword(p string) string {
	if len(p) < 8 {
		return "Passwords need at least 8 characters."
	}
	if len(p) > 72 {
		return "Passwords can be at most 72 characters."
	}
	if strings.TrimSpace(p) == "" {
		return "That password is only whitespace."
	}
	return ""
}

// validateNewPassword is validatePassword plus a confirmation field.
//
// Used wherever the user is choosing a password they cannot immediately test by
// signing in -- a reset, or a change while already signed in. A typo there locks
// them out of the account they were trying to secure, so the second box is not
// ceremony.
func validateNewPassword(p, confirm string) string {
	if msg := validatePassword(p); msg != "" {
		return msg
	}
	if p != confirm {
		return "Those two passwords do not match."
	}
	return ""
}


// ═════════════════════════════════════════════════════════════════════════════
// dashboard.go
// ═════════════════════════════════════════════════════════════════════════════


// dashboardView is the four-card dashboard the mockup draws.
//
// The savings funds live on the Emergency Fund tab: putting money aside IS the
// emergency fund from the user's point of view, so a separate Savings page was
// two names for one idea. The insight list and the month-by-month table are still
// on /reports.
type dashboardView struct {
	view

	// Period.
	Month      string // "" means all time
	MonthLabel string
	Months     []string
	ThisMonth  string

	// Which of the four cards is open. "current" is the mockup's default.
	Tab string

	// Card 1 — Current Funds.
	Cash    money.Cents
	Balance []store.Point
	Trend   insight.Trend

	// Card 2 — Emergency Fund. This tab owns every savings fund, not just the
	// emergency one, so the whole grid and its forms live here.
	EmergencyFund store.Fund
	Runway        insight.Runway
	Funds         []fundCard
	Totals        store.Totals

	// Card 3 — Monthly Income. Actual first, the forecast range second.
	IncomeRange    insight.IncomeRange
	Monthly        []store.MonthPoint
	IncomeBySource []store.LabelTotal
	IncomeTotal    money.Cents

	// SpendTotal and IncomeTotal are what actually happened in the period.
	// The cards lead with these because a figure a user has just entered should
	// be the one they see -- a forecast range next to a fresh $500 expense reads
	// as the app having ignored them.
	SpendTotal money.Cents

	// The needs/wants split, shown under the expenses total. This is the
	// distinction the presentation calls YABA's differentiator, and the flag has
	// been collected on every expense since the first pass.
	Essential money.Cents
	NonEssent money.Cents

	// Card 4 — Expected Monthly Expenses.
	Buckets         []store.Bucket
	ExpenseRange    insight.ExpenseRange
	Allocation      store.AllocationSummary
	SpendByCategory []store.LabelTotal

	PendingReceipts int
	Charts          chartData
}

type chartData struct {
	BalanceLabels []string
	BalanceValues []float64
	TrendValues   []float64
	MonthLabels   []string
	MonthIncome   []float64
	MonthExpense  []float64

	// The two doughnuts. They sit on the Expected Monthly Income and Expected
	// Monthly Expenses tabs, which is where a breakdown belongs -- the Add
	// pages are for entry, not analysis.
	IncomeLabels []string
	IncomeValues []float64
	SpendLabels  []string
	SpendValues  []float64
}

// validTabs guards the tab name arriving from a query string or fragment.
var validTabs = map[string]bool{
	"current": true, "emergency": true, "income": true, "expenses": true,
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	ctx := r.Context()

	month := r.URL.Query().Get("month")
	if month != "" {
		if _, err := time.Parse(store.MonthLayout, month); err != nil {
			// An unparseable month from a hand-edited URL falls back to all time
			// rather than erroring the whole page.
			month = ""
		}
	}

	tab := r.URL.Query().Get("tab")
	if !validTabs[tab] {
		tab = "current"
	}

	v := dashboardView{
		view:      s.baseView(w, r, "Dashboard", "dashboard"),
		Month:     month,
		Tab:       tab,
		ThisMonth: store.Today()[:7],
	}
	v.MonthLabel = "All time"
	if month != "" {
		if t, err := time.Parse(store.MonthLayout, month); err == nil {
			v.MonthLabel = t.Format("January 2006")
		}
	}

	// Anything monthly falls back to the current calendar month, because a
	// recurring-expense budget for "all time" is not a meaningful thing.
	budgetMonth := month
	if budgetMonth == "" {
		budgetMonth = v.ThisMonth
	}

	var err error
	if v.Months, err = s.store.Months(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	// ── Card 1: Current Funds ──────────────────────────────────────────────
	// Cash is a balance, not a flow, so it is all-time however the period filter
	// is set. Scoping a balance to one month would simply be wrong.
	if v.Cash, err = s.store.Cash(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Balance, err = s.store.BalanceSeries(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	v.Trend = insight.FitTrend(v.Balance)

	// ── Card 3: Expected Monthly Income ────────────────────────────────────
	if v.Monthly, err = s.store.MonthlySeries(ctx, sc, 12); err != nil {
		s.serverError(w, r, err)
		return
	}
	v.IncomeRange = insight.EstimateMonthlyIncome(v.Monthly)
	if v.IncomeBySource, err = s.store.Breakdown(ctx, sc, store.KindIncome, month); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Actual totals for the selected period.
	periodTotals, err := s.store.Totals(ctx, sc, month)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v.IncomeTotal, v.SpendTotal = periodTotals.Income, periodTotals.Expense

	if v.Essential, v.NonEssent, err = s.store.EssentialSplit(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}

	// ── Card 4: Expected Monthly Expenses ──────────────────────────────────
	if v.Buckets, err = s.store.Buckets(ctx, sc, budgetMonth); err != nil {
		s.serverError(w, r, err)
		return
	}
	v.ExpenseRange = insight.EstimateMonthlyExpenses(v.Buckets)
	// CategoryBreakdown rather than Breakdown, so a split transaction reports
	// each of its line items under its own category.
	if v.SpendByCategory, err = s.store.CategoryBreakdown(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Allocation, err = s.store.AllocationsFor(ctx, sc, budgetMonth); err != nil {
		s.serverError(w, r, err)
		return
	}

	// ── Card 2: Emergency Fund ─────────────────────────────────────────────
	if v.EmergencyFund, err = s.store.EmergencyFund(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	essentialMonthly, err := s.store.EssentialMonthlyCost(ctx, sc, budgetMonth)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	withdrawals, err := s.store.FundWithdrawalHistory(ctx, sc, v.EmergencyFund.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v.Runway = insight.AssessEmergencyFund(v.EmergencyFund, withdrawals, essentialMonthly)

	// Every fund, with its own projection, for the grid on this tab.
	if v.Totals, err = s.store.Totals(ctx, sc, ""); err != nil {
		s.serverError(w, r, err)
		return
	}
	funds, err := s.store.ListFunds(ctx, sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rates, err := s.store.DepositRates(ctx, sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	for _, f := range funds {
		rate := rates[f.ID]
		v.Funds = append(v.Funds, fundCard{
			Fund:       f,
			Projection: insight.ProjectFund(f, insight.AverageMonthlyDeposit(rate.Total, rate.Months), now),
		})
	}

	if v.PendingReceipts, err = s.store.PendingReceiptCount(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	v.Charts = buildDashboardCharts(v)
	s.render(w, r, "dashboard.html", v)
}

func buildDashboardCharts(v dashboardView) chartData {
	c := chartData{
		BalanceLabels: []string{}, BalanceValues: []float64{}, TrendValues: []float64{},
		MonthLabels: []string{}, MonthIncome: []float64{}, MonthExpense: []float64{},
		IncomeLabels: []string{}, IncomeValues: []float64{},
		SpendLabels: []string{}, SpendValues: []float64{},
	}
	for _, p := range v.Balance {
		c.BalanceLabels = append(c.BalanceLabels, p.Date)
		c.BalanceValues = append(c.BalanceValues, p.Balance.Float())
	}
	if v.Trend.OK {
		c.TrendValues = v.Trend.Values
	}
	for _, m := range v.Monthly {
		if t, err := time.Parse(store.MonthLayout, m.Month); err == nil {
			c.MonthLabels = append(c.MonthLabels, t.Format("Jan 06"))
		} else {
			c.MonthLabels = append(c.MonthLabels, m.Month)
		}
		c.MonthIncome = append(c.MonthIncome, m.Income.Float())
		c.MonthExpense = append(c.MonthExpense, m.Expense.Float())
	}
	// Blank labels are grouped rather than drawn as an unnamed slice.
	for _, lt := range v.IncomeBySource {
		label := lt.Label
		if label == "" {
			label = "Unlabelled"
		}
		c.IncomeLabels = append(c.IncomeLabels, label)
		c.IncomeValues = append(c.IncomeValues, lt.Total.Float())
	}
	for _, lt := range v.SpendByCategory {
		label := lt.Label
		if label == "" {
			label = "Unlabelled"
		}
		c.SpendLabels = append(c.SpendLabels, label)
		c.SpendValues = append(c.SpendValues, lt.Total.Float())
	}
	return c
}

// fundCard pairs a savings fund with its projection.
type fundCard struct {
	store.Fund
	Projection insight.Projection
}

// handleSavingsRedirect keeps the old /savings path working.
//
// Savings moved onto the Emergency Fund tab, so rather than 404 a bookmark or an
// old link, send it to the panel that now does the job.
func (s *Server) handleSavingsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard?tab=emergency", http.StatusMovedPermanently)
}

// ── /reports ──────────────────────────────────────────────────────────────────

type reportsView struct {
	view

	Month      string
	MonthLabel string
	Months     []string

	Totals    store.Totals
	AllTime   store.Totals
	Essential money.Cents
	NonEssent money.Cents

	IncomeBySource  []store.LabelTotal
	SpendByCategory []store.LabelTotal
	Monthly         []store.MonthPoint

	Budgets      []store.Budget
	Categories   []string
	Observations []insight.Observation

	Charts reportCharts
}

type reportCharts struct {
	IncomeLabels    []string
	IncomeValues    []float64
	SpendLabels     []string
	SpendValues     []float64
	MonthLabels     []string
	MonthIncome     []float64
	MonthExpense    []float64
	EssentialLabels []string
	EssentialValues []float64
}

// handleReports holds everything that used to be stacked under the dashboard
// tabs: the plain-language observations, the category and source breakdowns, the
// month-by-month table and the per-category budget caps.
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	ctx := r.Context()

	month := r.URL.Query().Get("month")
	if month != "" {
		if _, err := time.Parse(store.MonthLayout, month); err != nil {
			month = ""
		}
	}

	v := reportsView{
		view:       s.baseView(w, r, "Reports", "reports"),
		Month:      month,
		MonthLabel: "All time",
	}
	if month != "" {
		if t, err := time.Parse(store.MonthLayout, month); err == nil {
			v.MonthLabel = t.Format("January 2006")
		}
	}

	var err error
	if v.Months, err = s.store.Months(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Totals, err = s.store.Totals(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.AllTime, err = s.store.Totals(ctx, sc, ""); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Essential, v.NonEssent, err = s.store.EssentialSplit(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.IncomeBySource, err = s.store.Breakdown(ctx, sc, store.KindIncome, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	// CategoryBreakdown rather than Breakdown, so a split transaction reports
	// each line item under its own category.
	if v.SpendByCategory, err = s.store.CategoryBreakdown(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Monthly, err = s.store.MonthlySeries(ctx, sc, 12); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Budgets, err = s.store.ListBudgets(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Categories, err = s.store.SpendCategories(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	buckets, err := s.store.Buckets(ctx, sc, store.Today()[:7])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	alloc, err := s.store.AllocationsFor(ctx, sc, store.Today()[:7])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ef, err := s.store.EmergencyFund(ctx, sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	essential, err := s.store.EssentialMonthlyCost(ctx, sc, store.Today()[:7])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	wd, err := s.store.FundWithdrawalHistory(ctx, sc, ef.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	runway := insight.AssessEmergencyFund(ef, wd, essential)

	v.Observations = insight.Observations(v.Totals, v.Essential, v.NonEssent, v.Budgets, v.Monthly)
	v.Observations = append(v.Observations, bucketObservations(alloc, buckets, runway)...)

	v.Charts = buildReportCharts(v)
	s.render(w, r, "reports.html", v)
}

func buildReportCharts(v reportsView) reportCharts {
	c := reportCharts{
		IncomeLabels: []string{}, IncomeValues: []float64{},
		SpendLabels: []string{}, SpendValues: []float64{},
		MonthLabels: []string{}, MonthIncome: []float64{}, MonthExpense: []float64{},
		EssentialLabels: []string{}, EssentialValues: []float64{},
	}
	for _, lt := range v.IncomeBySource {
		c.IncomeLabels = append(c.IncomeLabels, lt.Label)
		c.IncomeValues = append(c.IncomeValues, lt.Total.Float())
	}
	for _, lt := range v.SpendByCategory {
		c.SpendLabels = append(c.SpendLabels, lt.Label)
		c.SpendValues = append(c.SpendValues, lt.Total.Float())
	}
	for _, m := range v.Monthly {
		if t, err := time.Parse(store.MonthLayout, m.Month); err == nil {
			c.MonthLabels = append(c.MonthLabels, t.Format("Jan 06"))
		} else {
			c.MonthLabels = append(c.MonthLabels, m.Month)
		}
		c.MonthIncome = append(c.MonthIncome, m.Income.Float())
		c.MonthExpense = append(c.MonthExpense, m.Expense.Float())
	}
	if v.Essential > 0 || v.NonEssent > 0 {
		c.EssentialLabels = append(c.EssentialLabels, "Essential", "Non-essential")
		c.EssentialValues = append(c.EssentialValues, v.Essential.Float(), v.NonEssent.Float())
	}
	return c
}

// bucketObservations turns the funding position into the same kind of plain
// sentence the rest of the insight panel uses.
func bucketObservations(a store.AllocationSummary, buckets []store.Bucket, r insight.Runway) []insight.Observation {
	var out []insight.Observation

	if r.Warning != "" {
		out = append(out, insight.Observation{Severity: insight.Alert, Text: r.Warning})
	}

	if a.Required > 0 && a.Shortfall > 0 {
		// Name the highest-priority unfunded bucket: that is the one to act on,
		// and it is the whole point of ranking them.
		first := ""
		for _, b := range buckets {
			if b.Shortfall() > 0 {
				first = b.Name
				break
			}
		}
		text := "Recurring expenses are short by " + a.Shortfall.Display() + " this month."
		if first != "" {
			text = first + " is the highest priority expense still unfunded. " + text
		}
		out = append(out, insight.Observation{Severity: insight.Alert, Text: text})
	}

	if a.Required > 0 && a.Shortfall == 0 {
		out = append(out, insight.Observation{Severity: insight.Good,
			Text: "Every recurring expense is funded for this month."})
	}

	if a.Unassigned > 0 && a.Shortfall == 0 {
		out = append(out, insight.Observation{Severity: insight.Info,
			Text: a.Unassigned.Display() + " of income is not committed to a recurring expense."})
	}

	return out
}

// ── notifications ─────────────────────────────────────────────────────────────

// handleNotifications is polled by the page to deliver toasts.
//
// Read-and-clear, so a message is shown exactly once even with two tabs open.
// This is what makes "gentle notification ... pops up, lives for a few seconds,
// then makes itself go away" work for a job that finished while the user was on
// a different page -- or, because the rows persist, on a different day.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	notes, err := s.store.TakeNotifications(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pending, err := s.store.PendingReceiptCount(r.Context(), scopeOf(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	type payload struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		Link string `json:"link"`
	}
	out := struct {
		Notifications []payload `json:"notifications"`
		Pending       int       `json:"pending"`
	}{Notifications: []payload{}, Pending: pending}

	for _, n := range notes {
		out.Notifications = append(out.Notifications, payload{Kind: n.Kind, Text: n.Text, Link: n.Link})
	}

	s.writeJSON(w, r, out)
}


// ═════════════════════════════════════════════════════════════════════════════
// transactions.go
// ═════════════════════════════════════════════════════════════════════════════


// transactionsView backs the transaction list.
type transactionsView struct {
	view

	Transactions []store.Transaction
	Filter       store.Filter

	// Pagination. The old page had none: it selected every row the user had
	// ever created and rendered all of them.
	Page       int
	TotalPages int
	Total      int
	From       int
	To         int
	PrevQuery  string
	NextQuery  string

	Months     []string
	Kinds      []kindOption
	SearchText string
}

type kindOption struct {
	Value    string
	Label    string
	Selected bool
}

func (s *Server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	q := r.URL.Query()

	kind := store.Kind(q.Get("type"))
	if kind != "" && !kind.Valid() {
		// An unknown value in the query string shows everything rather than
		// erroring; the old code fell through its switch to the combined view,
		// so this keeps the same forgiving behaviour deliberately.
		kind = ""
	}

	month := q.Get("month")
	if month != "" {
		if _, err := time.Parse(store.MonthLayout, month); err != nil {
			month = ""
		}
	}

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	f := store.Filter{
		Kind:   kind,
		Month:  month,
		Search: q.Get("q"),
		Limit:  store.DefaultPageSize,
		Offset: (page - 1) * store.DefaultPageSize,
	}

	txs, total, err := s.store.List(r.Context(), sc, f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	totalPages := (total + store.DefaultPageSize - 1) / store.DefaultPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	// A page number past the end (from a stale bookmark, or after deleting
	// rows) would otherwise render an empty table with no explanation.
	if page > totalPages {
		http.Redirect(w, r, "/transactions?"+withPage(q, totalPages), http.StatusSeeOther)
		return
	}

	v := transactionsView{
		view:         s.baseView(w, r, "Transactions", "transactions"),
		Transactions: txs,
		Filter:       f,
		Page:         page,
		TotalPages:   totalPages,
		Total:        total,
		SearchText:   q.Get("q"),
	}
	if total > 0 {
		v.From = f.Offset + 1
		v.To = f.Offset + len(txs)
	}
	if page > 1 {
		v.PrevQuery = withPage(q, page-1)
	}
	if page < totalPages {
		v.NextQuery = withPage(q, page+1)
	}

	if v.Months, err = s.store.Months(r.Context(), sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	for _, k := range []struct{ v, l string }{
		{"", "All types"},
		{string(store.KindIncome), "Income"},
		{string(store.KindExpense), "Expenses"},
		{string(store.KindFundDeposit), "To savings"},
		{string(store.KindFundWithdrawal), "From savings"},
	} {
		v.Kinds = append(v.Kinds, kindOption{
			Value: k.v, Label: k.l, Selected: k.v == string(kind),
		})
	}

	s.render(w, r, "transactions.html", v)
}

// withPage rebuilds the query string with a different page number, preserving
// the active filters so paging does not reset them.
func withPage(q map[string][]string, page int) string {
	out := make([]string, 0, 4)
	for _, key := range []string{"type", "month", "q"} {
		if vs, ok := q[key]; ok && len(vs) > 0 && vs[0] != "" {
			out = append(out, key+"="+urlEscape(vs[0]))
		}
	}
	out = append(out, "page="+strconv.Itoa(page))
	return strings.Join(out, "&")
}

func urlEscape(s string) string {
	return strings.NewReplacer(
		"&", "%26", "=", "%3D", "?", "%3F", "#", "%23",
		" ", "%20", "+", "%2B", "%", "%25", "\"", "%22",
		"<", "%3C", ">", "%3E",
	).Replace(s)
}

// ── create and edit ───────────────────────────────────────────────────────────

// transactionFormView backs the shared add/edit form.
type transactionFormView struct {
	view

	// Editing is false for a new entry.
	Editing bool
	ID      int64
	Action  string

	Kind       store.Kind
	Label      string // What?
	Amount     string
	OccurredOn string // When?
	Essential  bool

	// The rest of the wireframe's five Ws.
	Payee string // Who?
	Place string // Where?
	Note  string // Why?

	BucketID int64
	Buckets  []store.Bucket

	// Items backs the optional line-item breakdown.
	Items []store.LineItem

	Categories []string
	Error      string

	// FormToken is a one-time token for the create path; empty when editing,
	// because an edit is already protected by the version check.
	FormToken string

	// ReceiptJobID is a receipt already uploaded and waiting to be described.
	// Carried through the form in a hidden field so it survives a validation
	// error -- otherwise correcting a typo in the amount would silently detach
	// the receipt the user came here to attach.
	ReceiptJobID   int64
	ReceiptName    string
	ReceiptMissing bool

	// Version is the row version this form was built from, submitted back as a
	// hidden field so a save can be refused if the row moved on meanwhile.
	Version int64
}

func (s *Server) handleTransactionForm(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	kind := store.Kind(r.URL.Query().Get("type"))
	if kind != store.KindIncome && kind != store.KindExpense {
		kind = store.KindExpense
	}

	v := transactionFormView{
		view:       s.baseView(w, r, "Add entry", "transactions"),
		Kind:       kind,
		OccurredOn: store.Today(),
		Essential:  true,
		Action:     "/transactions/new",
	}
	// Only the create path needs one. An edit cannot duplicate anything: it
	// carries a version, and the second save of the same version is refused.
	if r.PathValue("id") == "" {
		v.FormToken = s.issueFormToken(r, "transaction")
	}

	// A receipt was uploaded earlier and could not be read automatically, so the
	// notification sent the user here to finish it by hand. This parameter used
	// to be ignored, which made every upload a dead end: the file was stored, the
	// notification arrived, and the link opened an ordinary blank form.
	if raw := r.URL.Query().Get("receipt"); raw != "" && kind == store.KindExpense {
		jobID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Either it belongs to another budget, or somebody already turned it
			// into an expense. Say so instead of pretending the form is normal,
			// and do not reveal which.
			v.ReceiptMissing = true
		case err != nil:
			s.serverError(w, r, err)
			return
		default:
			v.ReceiptJobID = job.ID
			v.ReceiptName = job.OriginalName
			// Seed the description from the filename. Wrong often enough that it
			// stays editable, useful often enough to save typing.
			if v.Label == "" {
				v.Label = receiptLabel(job.OriginalName)
			}
		}
	}

	var err error

	// The edit route shares this handler, populated from the existing row.
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		t, err := s.store.ByID(r.Context(), sc, id)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if t.Kind.IsTransfer() {
			// Transfers are not editable here: changing one would desynchronise
			// the fund balance it contributes to.
			s.flashError(w, r, "Savings transfers can't be edited. Withdraw from the fund instead.")
			http.Redirect(w, r, "/transactions", http.StatusSeeOther)
			return
		}

		v.Editing = true
		v.ID = t.ID
		v.Kind = t.Kind
		v.Label = t.Label
		v.Amount = t.Amount.Input()
		v.OccurredOn = t.OccurredOn
		v.Essential = t.Essential == nil || *t.Essential
		v.Payee, v.Place, v.Note = t.Payee, t.Place, t.Note
		if t.BucketID != nil {
			v.BucketID = *t.BucketID
		}
		v.Action = fmt.Sprintf("/transactions/%d/edit", t.ID)
		v.Title = "Edit entry"
		v.Version = t.Version

		if v.Items, err = s.store.LineItems(r.Context(), sc, t.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	if v.Categories, err = s.store.SpendCategories(r.Context(), sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Buckets, err = s.store.BucketOptions(r.Context(), sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "transaction_form.html", v)
}

// transactionInput is the parsed, validated result of the add/edit form.
type transactionInput struct {
	kind       store.Kind
	label      string
	amount     money.Cents
	occurredOn string
	essential  bool
	payee      string
	place      string
	note       string
	bucketID   *int64
	items      []store.NewLineItem

	// version is the row version the form was rendered from. Zero means the
	// submission carried none.
	version int64
}

// parseTransactionForm validates every field and returns a message suitable
// for showing the user.
//
// The old handlers did strconv.ParseFloat and, on failure, redirected back to
// an empty form with no explanation and the user's input discarded. Amounts
// were never checked for sign, so negative income was accepted.
func parseTransactionForm(r *http.Request) (transactionInput, string) {
	kind := store.Kind(r.PostFormValue("kind"))
	if kind != store.KindIncome && kind != store.KindExpense {
		return transactionInput{}, "Choose whether this is income or an expense."
	}
	return parseTransactionFormFor(r, kind)
}

// parseTransactionFormFor validates the form with the kind imposed by the caller
// rather than read from the request.
//
// The dedicated /income and /expense pages use this, which is what makes them
// genuinely single-purpose: a hand-edited POST cannot submit an expense to the
// income route and have it stored as income.
func parseTransactionFormFor(r *http.Request, kind store.Kind) (transactionInput, string) {
	var in transactionInput
	in.kind = kind

	in.label = strings.TrimSpace(r.PostFormValue("label"))
	if in.label == "" {
		if in.kind == store.KindIncome {
			return in, "Give the income a source, for example \"Salary\"."
		}
		return in, "Give the expense a category, for example \"Food\"."
	}

	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		return in, "Enter an amount greater than zero, using at most two decimal places."
	}
	in.amount = amount

	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		return in, "Enter a date in YYYY-MM-DD form."
	}
	in.occurredOn = date

	in.essential = r.PostFormValue("essential") != "no"

	// The three optional Ws. Trimmed but not required: an expense with only a
	// category and an amount is still a valid expense, and refusing to save one
	// would make the form tedious to use for a coffee.
	in.payee = strings.TrimSpace(r.PostFormValue("payee"))
	in.place = strings.TrimSpace(r.PostFormValue("place"))
	in.note = strings.TrimSpace(r.PostFormValue("note"))

	if raw := strings.TrimSpace(r.PostFormValue("bucket_id")); raw != "" && raw != "0" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return in, "That recurring expense could not be recognised."
		}
		// Ownership is verified in the store, not here: a handler check would be
		// one more place to forget it.
		in.bucketID = &id
	}

	// The version the editor was shown, used as a compare-and-swap on save. A
	// missing or unparseable value becomes 0, which means "do not check" -- the
	// pre-existing behaviour, so an old bookmarked form still saves rather than
	// failing in a way nobody could interpret.
	if raw := strings.TrimSpace(r.PostFormValue("version")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			in.version = v
		}
	}

	// Line items arrive as parallel arrays from repeated form fields.
	items, msg := parseLineItems(r, in.amount)
	if msg != "" {
		return in, msg
	}
	in.items = items

	return in, ""
}

// parseLineItems reads the optional breakdown rows.
//
// The wireframe asks for "a breakdown on what categories the individual line
// items in the transaction belong to". Rows are only accepted if they carry an
// amount, so the blank spare row the form always renders is ignored rather than
// rejected.
func parseLineItems(r *http.Request, total money.Cents) ([]store.NewLineItem, string) {
	descs := r.PostForm["item_description"]
	cats := r.PostForm["item_category"]
	amounts := r.PostForm["item_amount"]

	var items []store.NewLineItem
	var sum money.Cents

	for i, raw := range amounts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		amount, err := money.ParsePositive(raw)
		if err != nil {
			return nil, "Every line item needs an amount greater than zero."
		}
		it := store.NewLineItem{Amount: amount}
		if i < len(descs) {
			it.Description = strings.TrimSpace(descs[i])
		}
		if i < len(cats) {
			it.Category = strings.TrimSpace(cats[i])
		}
		items = append(items, it)
		sum += amount
	}

	if len(items) == 0 {
		return nil, ""
	}
	// Requiring the lines to reconcile is what keeps the category breakdown and
	// the headline total telling the same story. Letting them disagree would
	// leave no way to know which figure is right.
	if sum != total {
		return nil, fmt.Sprintf("The line items add up to %s but the total is %s.",
			sum.Display(), total.Display())
	}
	return items, ""
}

func (s *Server) handleTransactionCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	sc := scopeOf(r)
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, false, 0)
		return
	}

	if s.duplicateSubmit(r, user.ID) {
		s.flashSuccess(w, r, "That entry was already saved.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	n := in.toNewTransaction()

	// An expense may arrive with a receipt attached in the same submission. This
	// path is synchronous because the user is already waiting on the form; the
	// asynchronous queue is for the standalone import flow.
	if in.kind == store.KindExpense {
		path, name, err := s.saveReceipt(r, user.ID)
		if err != nil {
			s.rerenderTransactionForm(w, r, in, err.Error(), false, 0)
			return
		}
		n.ReceiptPath, n.ReceiptName = path, name
	}

	// Or it may be finishing a receipt uploaded earlier. Validate before the
	// insert so a stolen or already-used id fails without leaving a stray
	// expense behind.
	//
	// A file uploaded in this same submission wins: the user picked it just now,
	// which is a clearer statement of intent than a hidden field they never saw.
	var attachJobID int64
	if in.kind == store.KindExpense && n.ReceiptPath == "" {
		if raw := r.PostFormValue("receipt_job"); raw != "" {
			jobID, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				s.badRequest(w, "That receipt reference was not valid.")
				return
			}
			if _, err := s.store.UnattachedReceipt(r.Context(), sc, jobID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					s.rerenderTransactionForm(w, r, in,
						"That receipt has already been used, or is no longer available.", false, 0)
					return
				}
				s.serverError(w, r, err)
				return
			}
			attachJobID = jobID
		}
	}

	txID, err := s.store.Add(r.Context(), sc, n)
	if errors.Is(err, store.ErrNotFound) {
		s.rerenderTransactionForm(w, r, in, "That recurring expense no longer exists.", false, 0)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if attachJobID != 0 {
		if err := s.store.AttachReceipt(r.Context(), sc, attachJobID, txID); err != nil {
			// The expense is saved either way, so this reports rather than fails.
			// Losing the attachment silently would leave the receipt looking
			// unused and the expense looking undocumented.
			s.flashError(w, r, "Saved, but the receipt could not be attached to it.")
		}
	}

	if len(in.items) > 0 {
		if err := s.store.SetLineItems(r.Context(), sc, txID, in.items); err != nil {
			// The transaction saved; only the breakdown failed. Say so rather
			// than pretending everything worked.
			s.flashError(w, r, "Saved, but the line items could not be stored: "+err.Error())
		}
	}

	// Income funds the priority list, and a bucket-attributed expense changes
	// what a variable bucket costs. Either way the waterfall needs re-pouring.
	if err := s.store.ReallocateMonthOf(r.Context(), sc, in.occurredOn); err != nil {
		s.flashError(w, r, "Saved, but the expense funding could not be recalculated.")
	}

	s.flashSuccess(w, r, fmt.Sprintf("%s of %s saved.", in.kind.Label(), in.amount.Display()))
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleTransactionUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, true, id)
		return
	}

	err = s.store.Update(r.Context(), sc, id, in.toNewTransaction())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		// Somebody else saved this row while this form was open. Their version
		// stands and this one is refused, which is the only choice that cannot
		// lose work silently: overwriting would discard theirs, and merging two
		// sets of amounts without asking would invent a third answer neither
		// person entered.
		s.conflictOnUpdate(w, r, sc, id, in)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Passing an empty slice clears the breakdown, which is how a user removes
	// line items: they blank the rows and save.
	if err := s.store.SetLineItems(r.Context(), sc, id, in.items); err != nil {
		s.flashError(w, r, "Updated, but the line items could not be stored: "+err.Error())
	}
	if err := s.store.ReallocateMonthOf(r.Context(), sc, in.occurredOn); err != nil {
		s.flashError(w, r, "Updated, but the expense funding could not be recalculated.")
	}

	s.flashSuccess(w, r, "Entry updated.")
	http.Redirect(w, r, "/transactions", http.StatusSeeOther)
}

func (s *Server) handleTransactionDelete(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Read the date before deleting, so the right month can be recalculated.
	month := ""
	if t, e := s.store.ByID(r.Context(), sc, id); e == nil {
		month = t.OccurredOn
	}

	err = s.store.Delete(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		// Covers both "no such row" and "belongs to another user". The old
		// handler ran DELETE with the id and username in the WHERE clause but
		// reported nothing either way, so a user could not tell a failed
		// delete from a successful one.
		s.flashError(w, r, "That entry no longer exists.")
	} else if err != nil {
		s.serverError(w, r, err)
		return
	} else {
		// Deleting an income unwinds its allocations through the foreign key,
		// but the remaining income has to be re-poured over the priority list.
		if err := s.store.ReallocateMonthOf(r.Context(), sc, month); err != nil {
			s.flashError(w, r, "Deleted, but the expense funding could not be recalculated.")
		}
		s.flashSuccess(w, r, "Entry deleted.")
	}

	back := "/transactions"
	if q := r.PostFormValue("back"); strings.HasPrefix(q, "/transactions") {
		// Only same-path returns are honoured, so the redirect cannot be
		// turned into an open redirect to another site.
		back = q
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) rerenderTransactionForm(w http.ResponseWriter, r *http.Request, in transactionInput, msg string, editing bool, id int64) {
	// Echo back whatever version the submission carried, so an ordinary
	// validation error does not turn into a spurious conflict on the retry.
	s.rerenderTransactionFormAt(w, r, in, msg, editing, id, in.version)
}

// rerenderTransactionFormAt is rerenderTransactionForm with the row version
// stated explicitly, for the conflict case where the form must be re-armed with
// the version somebody else just created.
func (s *Server) rerenderTransactionFormAt(w http.ResponseWriter, r *http.Request,
	in transactionInput, msg string, editing bool, id, version int64) {
	sc := scopeOf(r)
	kind := in.kind
	if kind != store.KindIncome && kind != store.KindExpense {
		kind = store.KindExpense
	}

	v := transactionFormView{
		view:       s.baseView(w, r, "Add entry", "transactions"),
		Editing:    editing,
		ID:         id,
		Kind:       kind,
		Label:      r.PostFormValue("label"),
		Amount:     r.PostFormValue("amount"),
		OccurredOn: r.PostFormValue("date"),
		Essential:  r.PostFormValue("essential") != "no",
		Payee:      r.PostFormValue("payee"),
		Place:      r.PostFormValue("place"),
		Note:       r.PostFormValue("note"),
		Error:      msg,
		Action:     "/transactions/new",
		Version:    version,
	}
	if in.bucketID != nil {
		v.BucketID = *in.bucketID
	}
	// Echo the submitted line items back so a validation failure does not throw
	// away rows the user typed.
	for i, it := range in.items {
		v.Items = append(v.Items, store.LineItem{
			Description: it.Description, Category: it.Category,
			Amount: it.Amount, Position: i,
		})
	}
	// The token is not consumed until validation passes, so the same one is
	// still good and goes back into the re-rendered form.
	v.FormToken = strings.TrimSpace(r.PostFormValue("form_token"))

	// Keep the pending receipt across a validation failure. Re-read from the
	// store rather than echoing a posted filename, so the name shown on the page
	// is the one on the job and not one somebody typed into the form.
	if raw := r.PostFormValue("receipt_job"); raw != "" {
		if jobID, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID); err == nil {
				v.ReceiptJobID = job.ID
				v.ReceiptName = job.OriginalName
			}
		}
	}

	if editing {
		v.Action = fmt.Sprintf("/transactions/%d/edit", id)
		v.Title = "Edit entry"
	}
	if v.OccurredOn == "" {
		v.OccurredOn = store.Today()
	}
	if cats, err := s.store.SpendCategories(r.Context(), sc); err == nil {
		v.Categories = cats
	}
	if buckets, err := s.store.BucketOptions(r.Context(), sc); err == nil {
		v.Buckets = buckets
	}

	s.renderStatus(w, r, http.StatusBadRequest, "transaction_form.html", v)
}

// ── CSV export ────────────────────────────────────────────────────────────────

// handleExportCSV streams the filtered transactions as a spreadsheet.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	q := r.URL.Query()

	kind := store.Kind(q.Get("type"))
	if kind != "" && !kind.Valid() {
		kind = ""
	}
	month := q.Get("month")
	if month != "" {
		if _, err := time.Parse(store.MonthLayout, month); err != nil {
			month = ""
		}
	}

	txs, err := s.store.All(r.Context(), sc, store.Filter{
		Kind:   kind,
		Month:  month,
		Search: q.Get("q"),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	filename := "yaba-transactions"
	if month != "" {
		filename += "-" + month
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.csv"`)

	cw := csv.NewWriter(w)
	defer cw.Flush()

	// "Added by" is always a column here, unlike in the on-screen table where it
	// is hidden for a solo user. A CSV is archival and often opened months later
	// in another tool, so a stable column order is worth more than a tidy one.
	_ = cw.Write([]string{
		"Date", "Type", "Description", "Amount", "Effect on cash", "Essential",
		"Fund", "Added by",
	})
	for _, t := range txs {
		essential := ""
		if t.Essential != nil {
			if *t.Essential {
				essential = "Essential"
			} else {
				essential = "Non-essential"
			}
		}
		// Amounts are written as plain decimals with no currency symbol or
		// thousands separator, because a spreadsheet will not parse "$1,234.56"
		// as a number.
		_ = cw.Write([]string{
			t.OccurredOn,
			t.Kind.Label(),
			csvSafe(t.Label),
			t.Amount.Input(),
			t.SignedAmount().Input(),
			essential,
			csvSafe(t.FundName),
			csvSafe(t.AddedBy),
		})
	}
}

// csvSafe neutralises spreadsheet formula injection.
//
// A label like "=1+1" or "@SUM(A1:A9)" is executed as a formula when the file
// is opened in Excel or Sheets, which is a genuine attack path in any app that
// lets a user type text and later exports it. Prefixing with an apostrophe
// makes the cell literal text.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// toNewTransaction converts validated form input into the store's input type.
//
// Extracted because create and edit both need the identical mapping, and having
// it in one place is what stops the two paths drifting -- the old code had an
// edit path that quietly dropped fields the create path saved.
func (in transactionInput) toNewTransaction() store.NewTransaction {
	n := store.NewTransaction{
		Kind:       in.kind,
		Label:      in.label,
		Amount:     in.amount,
		OccurredOn: in.occurredOn,
		Payee:      in.payee,
		Place:      in.place,
		Note:       in.note,
		BucketID:   in.bucketID,
		Version:    in.version,
	}
	if in.kind == store.KindExpense {
		e := in.essential
		n.Essential = &e
	} else {
		// Income has no essential flag and no recurring-expense bucket. Clearing
		// them here means a form that leaves the fields populated when switching
		// type cannot write a nonsensical row.
		n.BucketID = nil
	}
	return n
}


// ═════════════════════════════════════════════════════════════════════════════
// entry.go
// ═════════════════════════════════════════════════════════════════════════════


// entryView backs the two dedicated entry pages, /income and /expense.
//
// One view type for both, but two templates and two routes. That split is the
// point: the brief asks that "the user should never feel confused about whether
// they are adding income or expense", and the surest way to achieve that is for
// the income page to have no expense fields on it at all -- not hidden ones, not
// a toggle, none. A shared type keeps the handlers from drifting while the
// templates stay single-purpose.
type entryView struct {
	view

	// Kind is what this page adds. The template does not offer a choice.
	Kind store.Kind

	// Form state, echoed back after a validation failure so nothing typed is
	// lost.
	Label      string
	Amount     string
	OccurredOn string
	Payee      string
	Place      string
	Note       string
	Essential  bool
	BucketID   int64
	Error      string

	// Step is "choose" or "manual" on the expense page. Empty on income, which
	// has only one route in.
	Step string

	// Suggestions for the label field, and buckets an expense can pay towards.
	//
	// Deliberately nothing else. These pages carry the form alone: the doughnut
	// charts and the recent lists now live on the dashboard's Expected Monthly
	// Income and Expected Monthly Expenses tabs, so the queries that fed them
	// here are gone rather than being run and discarded.
	Categories []string
	Buckets    []store.Bucket

	// FormToken is a one-time token, so a double click or a back-then-save does
	// not record the same money twice.
	FormToken string

	// Waiting are receipts already uploaded and processed that nobody has turned
	// into an expense yet.
	//
	// This list is what stops an upload being a dead end. Until it existed the
	// only route back to a stored receipt was its notification, and a
	// notification that is dismissed, cleared on another device or simply
	// scrolled past left the file unreachable from anywhere in the interface.
	Waiting []store.ReceiptJob
}

// handleIncomePage is GET /income.
func (s *Server) handleIncomePage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.buildEntryView(w, r, store.KindIncome, "Add Income", "income")
	if !ok {
		return
	}
	s.render(w, r, "income.html", v)
}

// handleExpensePage is GET /expense.
//
// Opens on the Manual-or-Upload chooser rather than a form, as specified: the
// user picks how they want to add the expense before seeing any fields.
func (s *Server) handleExpensePage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.buildEntryView(w, r, store.KindExpense, "Add Expense", "expense")
	if !ok {
		return
	}

	v.Step = "choose"
	if r.URL.Query().Get("step") == "manual" {
		v.Step = "manual"
	}

	s.render(w, r, "expense.html", v)
}

// buildEntryView loads everything both pages need. Reports false when it has
// already written an error response.
func (s *Server) buildEntryView(w http.ResponseWriter, r *http.Request, kind store.Kind, title, nav string) (entryView, bool) {
	sc := scopeOf(r)
	ctx := r.Context()

	v := entryView{
		view:       s.baseView(w, r, title, nav),
		Kind:       kind,
		OccurredOn: store.Today(),
		Essential:  true,
		FormToken:  s.issueFormToken(r, "entry"),
	}

	var err error
	if v.Categories, err = s.store.SpendCategories(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return v, false
	}
	if kind == store.KindExpense {
		if v.Buckets, err = s.store.BucketOptions(ctx, sc); err != nil {
			s.serverError(w, r, err)
			return v, false
		}
		if v.Waiting, err = s.store.UnattachedReceipts(ctx, sc, 10); err != nil {
			s.serverError(w, r, err)
			return v, false
		}
	}

	return v, true
}

// handleIncomeCreate is POST /income.
func (s *Server) handleIncomeCreate(w http.ResponseWriter, r *http.Request) {
	s.createEntry(w, r, store.KindIncome)
}

// handleExpenseCreate is POST /expense.
func (s *Server) handleExpenseCreate(w http.ResponseWriter, r *http.Request) {
	s.createEntry(w, r, store.KindExpense)
}

// createEntry saves one entry of a fixed kind.
//
// The kind comes from the route, never from the form. That is what makes the two
// pages genuinely separate: a hand-edited request cannot post an expense to the
// income page and have it accepted as income.
func (s *Server) createEntry(w http.ResponseWriter, r *http.Request, kind store.Kind) {
	user := mustUser(r)
	sc := scopeOf(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	in, msg := parseTransactionFormFor(r, kind)
	if msg != "" {
		s.rerenderEntry(w, r, kind, in, msg)
		return
	}

	// Validated, so this is a real submission — spend the token. If it has
	// already been spent, this is the same form arriving twice: the first one
	// worked, so send them to the result rather than showing an error for
	// something that succeeded, and above all do not record the money twice.
	if s.duplicateSubmit(r, user.ID) {
		s.flashSuccess(w, r, "That entry was already saved.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	n := in.toNewTransaction()

	// An expense may carry a receipt in the same submission. Income never can,
	// so the field is not on that page and this branch does not run for it.
	if kind == store.KindExpense {
		path, name, err := s.saveReceipt(r, user.ID)
		if err != nil {
			s.rerenderEntry(w, r, kind, in, err.Error())
			return
		}
		n.ReceiptPath, n.ReceiptName = path, name
	}

	txID, err := s.store.Add(r.Context(), sc, n)
	if errors.Is(err, store.ErrNotFound) {
		s.rerenderEntry(w, r, kind, in, "That recurring expense no longer exists.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if len(in.items) > 0 {
		if err := s.store.SetLineItems(r.Context(), sc, txID, in.items); err != nil {
			s.flashError(w, r, "Saved, but the line items could not be stored: "+err.Error())
		}
	}

	// Income funds the priority list, and a bucket-attributed expense changes
	// what a variable bucket costs. Either way the waterfall needs re-pouring,
	// which is what keeps the dashboard figures correct the moment this returns.
	if err := s.store.ReallocateMonthOf(r.Context(), sc, in.occurredOn); err != nil {
		s.flashError(w, r, "Saved, but the expense funding could not be recalculated.")
	}

	verb := "Income"
	if kind == store.KindExpense {
		verb = "Expense"
	}
	s.flashSuccess(w, r, verb+" of "+in.amount.Display()+" saved.")

	// Back to the same page, so the chart and the recent list visibly update and
	// another entry can be added without navigating.
	if kind == store.KindIncome {
		http.Redirect(w, r, "/income", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/expense?step=manual", http.StatusSeeOther)
}

// rerenderEntry redisplays the page with an error and the submitted values.
func (s *Server) rerenderEntry(w http.ResponseWriter, r *http.Request, kind store.Kind, in transactionInput, msg string) {
	title, nav, page := "Add Income", "income", "income.html"
	if kind == store.KindExpense {
		title, nav, page = "Add Expense", "expense", "expense.html"
	}

	v, ok := s.buildEntryView(w, r, kind, title, nav)
	if !ok {
		return
	}

	v.Error = msg
	v.Label = r.PostFormValue("label")
	v.Amount = r.PostFormValue("amount")
	v.OccurredOn = r.PostFormValue("date")
	v.Payee = r.PostFormValue("payee")
	v.Place = r.PostFormValue("place")
	v.Note = r.PostFormValue("note")
	v.Essential = r.PostFormValue("essential") != "no"
	if in.bucketID != nil {
		v.BucketID = *in.bucketID
	}
	if v.OccurredOn == "" {
		v.OccurredOn = store.Today()
	}
	if kind == store.KindExpense {
		v.Step = "manual"
	}

	s.renderStatus(w, r, http.StatusBadRequest, page, v)
}


// ═════════════════════════════════════════════════════════════════════════════
// funds.go
// ═════════════════════════════════════════════════════════════════════════════


// All fund handlers redirect back to the Emergency Fund tab, so the user lands
// where they were rather than at the top of the dashboard.
//
// A query parameter rather than a fragment: a fragment is never sent to the
// server, so the tab would have to be restored by JavaScript alone. ?tab= means
// the correct panel is chosen server-side and is right even with scripting off.
const savingsAnchor = "/dashboard?tab=emergency"

func (s *Server) handleFundCreate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.flashError(w, r, "Give the fund a name.")
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	// A goal is optional, so an empty field is zero rather than an error.
	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	if _, err := s.store.CreateFund(r.Context(), sc, name, goal, months); err != nil {
		s.flashError(w, r, err.Error())
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf("Fund %q created.", name))
	http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
}

func (s *Server) handleFundGoal(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	// A rename can arrive on the same form; an empty field leaves it alone.
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
		if err := s.store.RenameFund(r.Context(), sc, fundID, name); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			s.flashError(w, r, err.Error())
			http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
			return
		}
	}

	err := s.store.UpdateFundGoal(r.Context(), sc, fundID, goal, months)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That fund no longer exists.")
	} else if err != nil {
		s.flashError(w, r, err.Error())
	} else {
		s.flashSuccess(w, r, "Fund target updated.")
	}
	http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
}

func (s *Server) handleFundDeposit(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		s.flashError(w, r, "Enter an amount greater than zero to move into the fund.")
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}
	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		s.flashError(w, r, "Enter a date in YYYY-MM-DD form.")
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	// Note what is NOT read from this form: the fund's balance. The store
	// derives it. The old delete-fund handler read a fund_balance form field
	// and credited it as income, which is how the live database ended up with a
	// 50,000,000 deposit.
	err = s.store.Deposit(r.Context(), sc, fundID, amount, date)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case errors.Is(err, store.ErrInsufficientCash):
		// The store's message already names the available balance.
		s.flashError(w, r, "Not enough available cash. "+trimSentinel(err, store.ErrInsufficientCash))
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, fmt.Sprintf("%s moved into savings.", amount.Display()))
	}
	http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
}

func (s *Server) handleFundWithdraw(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		s.flashError(w, r, "Enter an amount greater than zero to take out of the fund.")
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}
	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		s.flashError(w, r, "Enter a date in YYYY-MM-DD form.")
		http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
		return
	}

	err = s.store.Withdraw(r.Context(), sc, fundID, amount, date)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case errors.Is(err, store.ErrInsufficientFund):
		s.flashError(w, r, "Not enough in that fund. "+trimSentinel(err, store.ErrInsufficientFund))
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, fmt.Sprintf("%s returned to available cash.", amount.Display()))
	}
	http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
}

// handleFundClose replaces the old /delete-fund route.
func (s *Server) handleFundClose(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	returned, err := s.store.CloseFund(r.Context(), sc, fundID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case err != nil:
		s.serverError(w, r, err)
		return
	case returned > 0:
		s.flashSuccess(w, r, fmt.Sprintf("Fund closed. %s returned to available cash.", returned.Display()))
	default:
		s.flashSuccess(w, r, "Fund closed.")
	}
	http.Redirect(w, r, savingsAnchor, http.StatusSeeOther)
}

// ── budgets ───────────────────────────────────────────────────────────────────

func (s *Server) handleBudgetSet(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	category := strings.TrimSpace(r.PostFormValue("category"))
	if category == "" {
		s.flashError(w, r, "Choose a category to budget.")
		http.Redirect(w, r, "/dashboard#budgets", http.StatusSeeOther)
		return
	}

	limit, err := money.ParsePositive(r.PostFormValue("limit"))
	if err != nil {
		s.flashError(w, r, "Enter a monthly limit greater than zero.")
		http.Redirect(w, r, "/dashboard#budgets", http.StatusSeeOther)
		return
	}

	if err := s.store.SetBudget(r.Context(), sc, category, limit); err != nil {
		s.flashError(w, r, err.Error())
	} else {
		s.flashSuccess(w, r, fmt.Sprintf("%s budget set to %s a month.", category, limit.Display()))
	}
	http.Redirect(w, r, "/dashboard#budgets", http.StatusSeeOther)
}

func (s *Server) handleBudgetDelete(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.DeleteBudget(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That budget no longer exists.")
	} else if err != nil {
		s.serverError(w, r, err)
		return
	} else {
		s.flashSuccess(w, r, "Budget removed.")
	}
	http.Redirect(w, r, "/dashboard#budgets", http.StatusSeeOther)
}

// ── shared helpers ────────────────────────────────────────────────────────────

// pathID parses the {id} path segment, writing a 404 and returning false when
// it is not a number.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// optionalAmount parses a field that may legitimately be blank.
func optionalAmount(s, field string) (money.Cents, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	c, err := money.Parse(s)
	if err != nil {
		return 0, "Enter a valid " + field + " amount, or leave it blank."
	}
	if c < 0 {
		return 0, "The " + field + " cannot be negative."
	}
	return c, ""
}

// optionalMonths parses a target horizon that may be blank.
func optionalMonths(s string) (int, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, "Enter the number of months as a whole number, or leave it blank."
	}
	if n > 600 {
		return 0, "That target is more than 50 years away."
	}
	return n, ""
}

// trimSentinel strips the sentinel prefix from a wrapped error so the message
// shown to the user does not repeat itself.
func trimSentinel(err, sentinel error) string {
	msg := err.Error()
	prefix := sentinel.Error() + ": "
	if i := strings.Index(msg, prefix); i >= 0 {
		return msg[i+len(prefix):]
	}
	return msg
}


// ═════════════════════════════════════════════════════════════════════════════
// buckets.go
// ═════════════════════════════════════════════════════════════════════════════


// expensesTab is where every bucket action returns the user, so they land back
// on the list they were editing rather than at the top of the dashboard.
const expensesTab = "/dashboard?tab=expenses#panel-expenses"

// parseBucketForm reads the recurring-expense form.
func parseBucketForm(r *http.Request) (store.NewBucket, string) {
	var n store.NewBucket

	n.Name = strings.TrimSpace(r.PostFormValue("name"))
	if n.Name == "" {
		return n, "Give the expense a name, for example \"Rent\"."
	}

	n.CostKind = store.CostKind(r.PostFormValue("cost_kind"))
	if !n.CostKind.Valid() {
		n.CostKind = store.CostFixed
	}
	n.Essential = r.PostFormValue("essential") == "yes"

	if n.CostKind == store.CostFixed {
		amount, err := money.ParsePositive(r.PostFormValue("amount"))
		if err != nil {
			return n, "A fixed monthly expense needs an amount greater than zero."
		}
		n.Fixed = amount
	}
	// A variable bucket's amount is derived from the transactions entered
	// against it, so any figure typed here is ignored rather than rejected --
	// the field stays on screen when the user switches kind.

	return n, ""
}

func (s *Server) handleBucketCreate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, expensesTab, http.StatusSeeOther)
		return
	}

	if _, err := s.store.CreateBucket(r.Context(), sc, n); err != nil {
		s.flashError(w, r, err.Error())
		http.Redirect(w, r, expensesTab, http.StatusSeeOther)
		return
	}

	// A new expense changes what the month requires, so the waterfall has to be
	// re-poured. Every mutation below does the same; the recalculation is
	// idempotent, so doing it unconditionally is safe and means no code path can
	// forget.
	s.reallocateNow(w, r)

	s.flashSuccess(w, r, fmt.Sprintf("%q added to your recurring expenses.", n.Name))
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

func (s *Server) handleBucketUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, expensesTab, http.StatusSeeOther)
		return
	}

	err := s.store.UpdateBucket(r.Context(), sc, id, n)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That expense no longer exists.")
	case err != nil:
		s.flashError(w, r, err.Error())
	default:
		s.reallocateNow(w, r)
		s.flashSuccess(w, r, "Recurring expense updated.")
	}
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

func (s *Server) handleBucketUp(w http.ResponseWriter, r *http.Request) {
	s.moveBucket(w, r, true)
}

func (s *Server) handleBucketDown(w http.ResponseWriter, r *http.Request) {
	s.moveBucket(w, r, false)
}

// moveBucket reorders the priority list.
//
// Reordering is not cosmetic here: priority decides which expense gets funded
// first when income arrives, so a move has to re-run the allocation.
func (s *Server) moveBucket(w http.ResponseWriter, r *http.Request, up bool) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.MoveBucket(r.Context(), sc, id, up)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That expense no longer exists.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.reallocateNow(w, r)
	}
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

func (s *Server) handleBucketArchive(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.ArchiveBucket(r.Context(), sc, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That expense no longer exists.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.reallocateNow(w, r)
		s.flashSuccess(w, r, "Recurring expense removed. Its history has been kept.")
	}
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

// handleReallocate lets the user force a recalculation.
//
// Nothing should normally need it, since every mutation recalculates. It exists
// because the allocation depends on the calendar month, so a month rolling over
// while the user was away leaves last month's figures on screen until something
// happens -- and having a button is friendlier than having to add an income to
// refresh the view.
func (s *Server) handleReallocate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	month := strings.TrimSpace(r.PostFormValue("month"))

	if err := s.store.Reallocate(r.Context(), sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flashSuccess(w, r, "Income re-allocated down the priority list.")
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

// reallocateNow re-pours the waterfall for the current month, reporting a
// failure to the user rather than leaving the page silently stale.
// The scope is resolved from the request rather than passed in. It used to take
// a userID, which is now the wrong type entirely; taking nothing at all is better
// than taking a Scope, because there is then no way for a caller to hand it a
// household other than the one the request is operating on.
func (s *Server) reallocateNow(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Reallocate(r.Context(), scopeOf(r), ""); err != nil {
		// Not fatal: the change the user asked for did happen. But the funding
		// figures they are about to look at are now wrong, so say so.
		s.flashError(w, r, "Saved, but the expense funding could not be recalculated. Try the Recalculate button.")
	}
}


// ═════════════════════════════════════════════════════════════════════════════
// receipts.go
// ═════════════════════════════════════════════════════════════════════════════


// allowedReceiptTypes maps a detected MIME type to the extension it is stored
// with. An allowlist, not a blocklist: anything not named here is refused.
var allowedReceiptTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
	"application/pdf": ".pdf",
}

func (s *Server) maxUploadBytes() int64 {
	mb := s.cfg.MaxUploadMB
	if mb <= 0 {
		mb = 5
	}
	return mb << 20
}

// saveReceipt stores an uploaded receipt and returns its path and original
// name. An absent file is not an error: the field is optional.
//
// The old handler had four separate problems, all fixed here:
//
//  1. filename := timestamp + "_" + user + "_" + handler.Filename, then
//     filepath.Join("./uploads", filename). handler.Filename is attacker
//     controlled, so a name containing path separators escaped the uploads
//     directory. Names are now generated from crypto/rand and the user's
//     original name is only ever stored in a database column.
//
//  2. No type check at all, so any file at all could be uploaded and later
//     served back. Now the content is sniffed and matched against an
//     allowlist -- the declared Content-Type is not trusted, since the client
//     chooses it.
//
//  3. It inserted a $0 expense whose category was the filename, which put a
//     junk slice into the spending pie chart for every receipt uploaded. The
//     receipt now attaches to a real expense with a real amount.
//
//  4. Files were written under a directory later exposed by a plain file
//     server. Receipts are now served only through handleReceipt.
func (s *Server) saveReceipt(r *http.Request, userID int64) (path, original string, err error) {
	if r.MultipartForm == nil {
		// Not a multipart submission, so no file was attached.
		return "", "", nil
	}

	file, header, err := r.FormFile("receipt")
	if errors.Is(err, http.ErrMissingFile) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("could not read the attached file")
	}
	defer file.Close()

	if header.Size == 0 {
		return "", "", nil
	}
	if header.Size > s.maxUploadBytes() {
		return "", "", fmt.Errorf("that file is larger than %d MB", s.cfg.MaxUploadMB)
	}

	// Sniff the first 512 bytes, which is what http.DetectContentType reads,
	// then rewind so the whole file still gets copied.
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", "", fmt.Errorf("could not read the attached file")
	}
	head = head[:n]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("could not read the attached file")
	}

	ext, ok := allowedReceiptTypes[normaliseMIME(http.DetectContentType(head))]
	if !ok {
		return "", "", fmt.Errorf("receipts must be a JPEG, PNG, WebP, GIF or PDF")
	}

	dir := s.cfg.UploadDir
	if dir == "" {
		dir = "uploads"
	}
	// Per-user subdirectory keeps one user's files from being listed alongside
	// another's, and keeps the directory from growing to tens of thousands of
	// entries in one flat folder.
	dir = filepath.Join(dir, strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}

	name, err := randomFilename(ext)
	if err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}
	full := filepath.Join(dir, name)

	// O_EXCL means a name collision fails rather than overwriting an existing
	// receipt. With 16 random bytes a collision is not realistic, but silently
	// overwriting someone's file would be the worst possible outcome.
	dst, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}
	defer dst.Close()

	// Cap the copy as well as checking header.Size: the declared size is a
	// hint, and LimitReader is what actually bounds what reaches the disk.
	if _, err := io.Copy(dst, io.LimitReader(file, s.maxUploadBytes())); err != nil {
		os.Remove(full)
		return "", "", fmt.Errorf("could not store the receipt")
	}

	// Stored with forward slashes whatever the local separator is.
	//
	// filepath.Join uses the OS separator, so a database written on Windows held
	// `uploads\16\abc.png`, which the same application on Linux cannot open --
	// there the backslashes are ordinary filename characters, so the whole thing
	// is one file that does not exist. One canonical form in the database,
	// converted back only when a file is actually opened.
	return filepath.ToSlash(full), cleanOriginalName(header.Filename), nil
}

// receiptFile turns a stored path back into one the local filesystem understands.
//
// The inverse of the ToSlash above. On Linux it is a no-op; on Windows it turns
// the stored uploads/16/abc.png into uploads\16\abc.png.
func receiptFile(stored string) string {
	return filepath.FromSlash(stored)
}

// handleImportRedirect keeps the old /import path working.
//
// The Manual-or-Upload chooser moved onto /expense, so rather than 404 any
// bookmark or in-page link that still points here, send them to the page that
// now does the job.
func (s *Server) handleImportRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/expense", http.StatusMovedPermanently)
}

// handleReceiptUpload stores the image and queues it, then returns immediately.
//
// This is the asynchronous path the wireframe specifies: "Picture gets put into
// a queue server side for processing and the user can go about the rest of their
// business." Nothing is parsed here, so the response time is the time to write
// one file and one row -- however slow processing later turns out to be.
func (s *Server) handleReceiptUpload(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	path, name, err := s.saveReceipt(r, user.ID)
	if err != nil {
		s.flashError(w, r, err.Error())
		http.Redirect(w, r, "/expense", http.StatusSeeOther)
		return
	}
	if path == "" {
		s.flashError(w, r, "Choose a receipt image or PDF to upload.")
		http.Redirect(w, r, "/expense", http.StatusSeeOther)
		return
	}

	if _, err := s.store.EnqueueReceipt(r.Context(), scopeOf(r), path, name); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Nudge the worker so it starts now rather than on its next tick. This never
	// blocks, so a busy worker cannot slow the upload response down.
	s.wakeWorker()

	s.flashSuccess(w, r,
		"Receipt uploaded. It is being processed in the background — carry on, and you will be told when it is ready.")
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleReceipt serves a stored receipt to the user who owns it.
// handleReceiptDiscard throws away an uploaded receipt nobody wants.
func (s *Server) handleReceiptDiscard(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	path, err := s.store.DiscardReceipt(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That receipt has already been used or is no longer there.")
		http.Redirect(w, r, "/expense", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The row is gone, so the file is now unreachable either way. Removing it is
	// housekeeping, not correctness, which is why a failure here is logged rather
	// than shown -- telling someone their receipt was discarded "but the file
	// could not be deleted" invites a worry they cannot act on.
	if path != "" {
		if err := os.Remove(receiptFile(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("receipts: discarded job %d but could not delete %s: %v", id, path, err)
		}
	}

	s.flashSuccess(w, r, "Receipt discarded.")
	http.Redirect(w, r, "/expense", http.StatusSeeOther)
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Ownership is checked by looking the transaction up scoped to this user,
	// so a guessed transaction id returns 404 rather than someone else's file.
	t, err := s.store.ByID(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.ReceiptPath == "") {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	f, err := os.Open(receiptFile(t.ReceiptPath))
	if err != nil {
		// The row survives even if the file is gone, so this is a 404 rather
		// than a 500.
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Content-Disposition: attachment, plus nosniff, means a stored file is
	// downloaded rather than rendered in the origin. That matters because an
	// SVG or HTML file rendered inline would run script with access to this
	// site's cookies -- the reason those types are not in the allowlist either.
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+safeDownloadName(t.ReceiptName, t.ReceiptPath)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(t.ReceiptPath), info.ModTime(), f)
}

// normaliseMIME strips any parameters from a detected content type, so
// "text/plain; charset=utf-8" compares as "text/plain".
func normaliseMIME(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.ToLower(s))
}

func randomFilename(ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b) + ext, nil
}

// cleanOriginalName keeps a readable version of the uploaded name for display
// only. It never touches the filesystem, but it is still stripped of path
// separators and control characters so it cannot mislead in the UI or break a
// Content-Disposition header.
// receiptLabel turns an uploaded filename into a first guess at a description.
//
// A guess, not an answer: "IMG_4821" tells nobody anything, so anything that
// looks like a camera's automatic name is left blank rather than filled with
// noise the user then has to delete. A name someone actually chose is usually
// worth offering.
func receiptLabel(original string) string {
	name := cleanOriginalName(original)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.NewReplacer("_", " ", "-", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")

	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}

	// Camera and screenshot defaults carry no information.
	lower := strings.ToLower(name)
	for _, prefix := range []string{"img", "image", "photo", "screenshot", "scan", "dsc", "pxl"} {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}
	// A name that is only digits and punctuation is a timestamp, not a label.
	if strings.IndexFunc(name, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}) < 0 {
		return ""
	}
	return name
}

func cleanOriginalName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '/' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "receipt"
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// safeDownloadName picks the filename offered to the browser.
func safeDownloadName(original, storedPath string) string {
	if n := cleanOriginalName(original); n != "receipt" {
		return n
	}
	return filepath.Base(storedPath)
}


// ═════════════════════════════════════════════════════════════════════════════
// household.go
// ═════════════════════════════════════════════════════════════════════════════


// Shared budgeting: the settings page, and the actions on it.
//
// Every handler here follows the same shape as the rest of the application --
// validate, act, flash, redirect -- so a failure is always visible to the user
// rather than logged and redirected as though it had worked.

// householdView backs /household.
type householdView struct {
	view

	Members []store.Member
	Pending []store.Invite

	// Roles are the roles an owner may assign, for the per-member selector.
	// Built from the store's constants rather than written out in the template,
	// so adding a role cannot leave the UI out of step with the model.
	Roles []store.Role

	// Activity is the household's recent history: who changed what, and when.
	Activity []store.AuditEntry

	// MailEnabled drives what the page says about delivery.
	//
	// It exists because the page used to state, in two hardcoded places, that
	// nothing is ever emailed -- which stopped being true the moment SMTP was
	// configured, and produced a page carrying a red "the email could not be
	// sent" beside a blue "nothing is emailed". Both cannot be right. Asking the
	// mailer means neither can drift from what the server actually does.
	MailEnabled bool
}

// assignableRoles excludes nothing: an owner may promote another member to
// owner, which is how ownership is handed over before somebody leaves.
func assignableRoles() []store.Role {
	return []store.Role{store.RoleOwner, store.RoleEditor, store.RoleViewer}
}

// handleHousehold shows who can see this budget.
//
// Readable by every member, including a viewer. Knowing who else has access to
// your finances is not a privilege -- somebody who can see the numbers should be
// able to see who else can.
func (s *Server) handleHousehold(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	v := householdView{
		view:        s.baseView(w, r, "Sharing", "household"),
		Roles:       assignableRoles(),
		MailEnabled: s.mail.Enabled(),
	}

	var err error
	if v.Members, err = s.store.Members(r.Context(), hh.ID, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Pending invitations are the owner's business: a viewer has no action to
	// take on them, and the addresses of people who have not yet accepted are
	// not theirs to read.
	if hh.Role.CanManageMembers() {
		if v.Pending, err = s.store.PendingInvites(r.Context(), hh.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	// The history is for every member, not only the owner: "who deleted the rent
	// entry?" is exactly the question an editor needs answered, and they can
	// already see all of this household's money.
	//
	// Invitation entries are the one exception, filtered out for anyone who
	// cannot manage members — for the same reason the pending list above is
	// hidden from them. An address that has not accepted yet is not theirs.
	activity, err := s.store.AuditLog(r.Context(), scopeOf(r), 40)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, e := range activity {
		if e.Entity == "invitation" && !hh.Role.CanManageMembers() {
			continue
		}
		v.Activity = append(v.Activity, e)
	}

	s.render(w, r, "household.html", v)
}

// handleHouseholdCreate makes a new shared budget and switches to it.
func (s *Server) handleHouseholdCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if _, err := s.store.CreateSharedHousehold(r.Context(), user.ID, name); err != nil {
		s.flashError(w, r, err.Error())
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf("Created %q. Invite someone to join it.", name))
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleHouseholdSwitch changes which budget the user is looking at.
//
// The id is validated by SwitchHousehold, whose UPDATE carries the membership
// test in its WHERE clause, so a hand-edited form cannot switch into a household
// the user does not belong to.
func (s *Server) handleHouseholdSwitch(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	id, err := strconv.ParseInt(r.PostFormValue("household_id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not a budget id.")
		return
	}

	switch err := s.store.SwitchHousehold(r.Context(), user.ID, id); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "You do not have access to that budget.")
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	// Back to the dashboard rather than to the referring page: the whole point
	// of switching is to look at different numbers, and every figure on the
	// previous page belonged to the household just left.
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleHouseholdRename renames the current shared budget.
func (s *Server) handleHouseholdRename(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	if err := s.store.RenameHousehold(r.Context(), hh.ID, r.PostFormValue("name")); err != nil {
		s.flashError(w, r, err.Error())
	} else {
		s.flashSuccess(w, r, "Name updated.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleHouseholdDelete removes a shared budget and everything in it.
func (s *Server) handleHouseholdDelete(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	switch err := s.store.DeleteHousehold(r.Context(), hh.ID); {
	case errors.Is(err, store.ErrPersonalHousehold):
		s.flashError(w, r, "This is your own budget, so it cannot be deleted.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf("Deleted %q. You are back in your own budget.", hh.Name))
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleHouseholdLeave is a member removing themselves from a shared budget.
func (s *Server) handleHouseholdLeave(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	switch err := s.store.LeaveHousehold(r.Context(), hh.ID, user.ID); {
	case errors.Is(err, store.ErrPersonalHousehold):
		s.flashError(w, r, "This is your own budget, so there is nothing to leave.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrLastOwner):
		// Deliberately specific. "You cannot leave" would be baffling; the user
		// needs to know the fix is to promote somebody first.
		s.flashError(w, r,
			"You are the only owner of this budget. Make someone else an owner first, "+
				"or delete the budget instead.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf("You have left %q.", hh.Name))
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// ── invitations ───────────────────────────────────────────────────────────────

// handleInviteCreate invites an email address to the current budget.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	role := store.Role(r.PostFormValue("role"))

	// The same address validation the login form uses, so "kushith" is rejected
	// here for the same reason and with the same wording it is rejected there.
	if msg := validateEmail(email); msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	// Inviting a personal budget makes no sense: it is one person's private
	// space. Offer the fix rather than just refusing.
	if hh.Personal {
		s.flashError(w, r,
			"This is your own private budget. Create a shared budget below, then invite people to that.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	if store.NormalizeEmail(email) == store.NormalizeEmail(user.Email) {
		s.flashError(w, r, "You are already in this budget.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	switch err := s.store.InviteMember(r.Context(), hh.ID, user.ID, email, role); {
	case errors.Is(err, store.ErrAlreadyMember):
		s.flashError(w, r, "That person is already in this budget.")
	case errors.Is(err, store.ErrInviteOpen):
		s.flashError(w, r, "They already have an invitation waiting.")
	case err != nil:
		s.flashError(w, r, err.Error())
	default:
		s.sendInvitation(w, r, store.NormalizeEmail(email), hh.Name, user.DisplayName, role)
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleInviteRevoke withdraws an invitation this household sent.
func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not an invitation id.")
		return
	}

	// The household id is part of the WHERE clause in RevokeInvite, so an owner
	// cannot cancel another household's invitation by guessing its id.
	switch err := s.store.RevokeInvite(r.Context(), id, hh.ID); {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That invitation is no longer waiting.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, "Invitation withdrawn.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleInviteAccept joins the household an invitation names.
//
// Not behind canManage: the user is not a member of that household yet, so a
// role check against their *current* household would be meaningless. Authority
// comes from the invitation matching their own email address, which AcceptInvite
// verifies inside its transaction.
func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not an invitation id.")
		return
	}

	switch err := s.store.AcceptInvite(r.Context(), id, user.ID, user.Email); {
	case errors.Is(err, store.ErrInviteExpired):
		// Distinguished from "no longer available" on purpose. The recipient did
		// nothing wrong and there is a specific thing that fixes it, so saying
		// "expired, ask them to send another" is more use than a dead end.
		s.flashError(w, r,
			"That invitation has expired. Ask them to send it again — invitations last 24 hours.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrNotFound):
		// Same message whether the invitation never existed, was withdrawn, or
		// belongs to somebody else -- a probing user learns nothing either way.
		s.flashError(w, r, "That invitation is no longer available.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.flashSuccess(w, r, "You have joined. This is the shared budget.")
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleInviteDecline refuses an invitation.
func (s *Server) handleInviteDecline(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not an invitation id.")
		return
	}

	if err := s.store.DeclineInvite(r.Context(), id, user.Email); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	s.flashSuccess(w, r, "Invitation declined.")
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// ── members ───────────────────────────────────────────────────────────────────

// handleMemberRole changes one member's role.
func (s *Server) handleMemberRole(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}

	target, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not a member id.")
		return
	}

	role := store.Role(r.PostFormValue("role"))
	if !role.Valid() {
		s.flashError(w, r, "Choose Owner, Editor or Viewer.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	switch err := s.store.SetRole(r.Context(), hh.ID, user.ID, target, role); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrLastOwner):
		s.flashError(w, r,
			"This budget needs at least one owner. Make someone else an owner first.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		if target == user.ID {
			// Demoting yourself takes effect on the next request, so say so --
			// otherwise the page reloads with fewer buttons and no explanation.
			s.flashSuccess(w, r, "Your own role is now "+strings.ToLower(role.Label())+".")
		} else {
			s.flashSuccess(w, r, "Role updated.")
		}
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleMemberRemove takes somebody out of the household.
//
// Their entries stay: the rows belong to the household and user_id is only
// attribution, so removing a member does not rewrite the budget's history.
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	target, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not a member id.")
		return
	}

	switch err := s.store.RemoveMember(r.Context(), hh.ID, user.ID, target); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrLastOwner):
		s.flashError(w, r,
			"This budget needs at least one owner. Make someone else an owner first.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, "Removed. Anything they entered stays in the budget.")
		if target == user.ID {
			// An owner who removed themselves is no longer looking at this
			// household; RemoveMember has already moved them home.
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// ── invitation delivery ───────────────────────────────────────────────────────

// sendInvitation emails the invitation and reports honestly what happened.
//
// Three outcomes, three different messages. The old code always said "there is
// no email, so let them know", which was true then and would be a lie now; and
// claiming "invitation sent" when SMTP is not configured would leave an owner
// waiting for a delivery that was never attempted. The owner needs to know
// whether they still have to tell the person themselves.
func (s *Server) sendInvitation(w http.ResponseWriter, r *http.Request,
	email, household, invitedBy string, role store.Role) {

	roleWord := strings.ToLower(role.Label())

	if !s.mail.Enabled() {
		s.flashSuccess(w, r, fmt.Sprintf(
			"%s is invited as a %s, and the invitation expires in 24 hours. "+
				"Email is not configured, so let them know — they will see it when they sign in.",
			email, roleWord))
		return
	}

	err := s.mail.Invitation(r.Context(), email, household, invitedBy,
		string(role), store.InviteTTL)
	if err != nil {
		// The invitation row exists and is perfectly usable, so this is not a
		// failure of the invitation -- only of its delivery. Saying so lets the
		// owner fall back to telling them directly instead of assuming it worked.
		log.Printf("mail: invitation to %s failed: %v", email, err)
		s.flashError(w, r, fmt.Sprintf(
			"%s is invited as a %s, but the email could not be sent. "+
				"Tell them to sign in within 24 hours, or resend it below.",
			email, roleWord))
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf(
		"Invitation emailed to %s as a %s. It expires in 24 hours.", email, roleWord))
}

// handleInviteResend gives an unanswered invitation another 24 hours and emails
// it again.
func (s *Server) handleInviteResend(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not an invitation id.")
		return
	}

	// The household id is passed in, so an owner of one budget cannot refresh an
	// invitation belonging to another.
	inv, err := s.store.ResendInvite(r.Context(), hh.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That invitation is no longer waiting for an answer.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.sendInvitation(w, r, inv.Email, inv.HouseholdName, user.DisplayName, inv.Role)
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleTransferOwnership hands the budget to another member.
func (s *Server) handleTransferOwnership(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	target, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not a member id.")
		return
	}

	switch err := s.store.TransferOwnership(r.Context(), hh.ID, user.ID, target); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrForbidden):
		s.flashError(w, r, "Only the owner can hand over a budget.")
	case err != nil:
		s.flashError(w, r, err.Error())
	default:
		s.flashSuccess(w, r,
			"Ownership handed over. You are now an editor of this budget, "+
				"so you can still add and change entries but not move savings.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// ── password reset ────────────────────────────────────────────────────────────

// forgotView backs both states of the Forgot password page.
type forgotView struct {
	view

	// Sent is true after a request, whether or not the address was recognised.
	Sent  bool
	Email string
	Error string

	// MailEnabled lets the page tell the truth about whether anything was sent.
	MailEnabled bool
}

// handleForgotRequest starts a password reset.
//
// The response is identical whether or not the address has an account. That is
// the whole security property of this endpoint: a different message, a different
// status code or a noticeably different response time would turn it into an
// oracle for which addresses are registered.
//
// So the work is deliberately arranged to be uniform. The token is created and
// the mail is sent in a goroutine, and the handler always renders the same
// "check your inbox" page immediately.
func (s *Server) handleForgotRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))

	v := forgotView{
		view:        s.baseView(w, r, "Forgot password", "forgot"),
		Email:       email,
		MailEnabled: s.mail.Enabled(),
	}

	if msg := validateEmail(email); msg != "" {
		v.Error = msg
		s.renderStatus(w, r, http.StatusBadRequest, "forgot.html", v)
		return
	}

	// Rate limited on IP alone, not on the address. Keying on the address would
	// let anyone lock a known user out of their own reset, and the thing being
	// limited here is a stranger asking for links, not a user proving anything.
	key := "reset|" + clientIP(r)
	retryIn, err := s.store.RateRetryIn(r.Context(), key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if retryIn > 0 {
		v.Error = "Too many reset requests. Try again " + retryPhrase(retryIn) + "."
		s.renderStatus(w, r, http.StatusTooManyRequests, "forgot.html", v)
		return
	}
	if err := s.store.RateFail(r.Context(), key); err != nil {
		log.Printf("reset: could not record the attempt: %v", err)
	}

	// Detached from the request context on purpose: this outlives the response,
	// which is what keeps the timing uniform. A context cancelled when the
	// handler returns would abort the send.
	go s.deliverReset(email)

	v.Sent = true
	s.render(w, r, "forgot.html", v)
}

// deliverReset does the part that must not affect the response.
func (s *Server) deliverReset(email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	user, _, err := s.store.CredentialsFor(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		// Nothing to do, and nothing to say. Logged at all only because a stream
		// of these is the signature of someone fishing for valid addresses.
		log.Printf("reset: no account for a requested address")
		return
	}
	if err != nil {
		log.Printf("reset: could not look up an account: %v", err)
		return
	}

	token, err := s.store.CreateReset(ctx, user.ID)
	if err != nil {
		log.Printf("reset: could not create a token for user %d: %v", user.ID, err)
		return
	}
	if err := s.mail.PasswordReset(ctx, user.Email, token, store.ResetTTL); err != nil {
		log.Printf("reset: could not email user %d: %v", user.ID, err)
	}
}

// resetView backs the "choose a new password" page.
type resetView struct {
	view

	Token string
	Email string

	// Invalid means the link is expired, already used, or was never real. The
	// page then shows an explanation and a link to ask for another, rather than a
	// form that is guaranteed to fail.
	Invalid bool
	Error   string
}

// handleResetForm shows the new-password form for a valid token.
func (s *Server) handleResetForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	v := resetView{view: s.baseView(w, r, "Choose a new password", "reset"), Token: token}

	user, err := s.store.ResetUser(r.Context(), token)
	if err != nil {
		v.Invalid = true
		s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
		return
	}
	v.Email = user.Email
	s.render(w, r, "reset.html", v)
}

// handleResetSubmit sets the new password.
func (s *Server) handleResetSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}
	token := r.PostFormValue("token")
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	v := resetView{view: s.baseView(w, r, "Choose a new password", "reset"), Token: token}

	// Resolve first, so an expired link is reported as expired rather than as a
	// password problem.
	user, err := s.store.ResetUser(r.Context(), token)
	if err != nil {
		v.Invalid = true
		s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
		return
	}
	v.Email = user.Email

	if msg := validateNewPassword(password, confirm); msg != "" {
		v.Error = msg
		s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// ConsumeReset burns the token, sets the hash and deletes every session for
	// the account in one transaction. Signing out all devices is the point: a
	// reset usually means somebody else may have had the old password.
	if _, err := s.store.ConsumeReset(r.Context(), token, string(hash)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Lost a race with another submission of the same link.
			v.Invalid = true
			s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
			return
		}
		s.serverError(w, r, err)
		return
	}

	log.Printf("reset: password changed for user %d, all sessions revoked", user.ID)
	s.flashSuccess(w, r, "Password changed. Please sign in with your new password.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── changing your own password ────────────────────────────────────────────────

type passwordView struct {
	view
	Error string

	// Others is how many other devices will be signed out, so the consequence is
	// stated before the button is pressed rather than discovered afterwards.
	Others int
}

func (s *Server) handlePasswordForm(w http.ResponseWriter, r *http.Request) {
	s.renderPasswordForm(w, r, http.StatusOK, "")
}

func (s *Server) renderPasswordForm(w http.ResponseWriter, r *http.Request, status int, msg string) {
	user := mustUser(r)
	v := passwordView{
		view:  s.baseView(w, r, "Change password", "password"),
		Error: msg,
	}
	if list, err := s.store.Sessions(r.Context(), user.ID, currentSession(r)); err == nil {
		for _, sess := range list {
			if !sess.Current {
				v.Others++
			}
		}
	}
	s.renderStatus(w, r, status, "password.html", v)
}

// handlePasswordChange replaces the password for the signed-in user.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return
	}
	current := r.PostFormValue("current")
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	// The current password is required even though the session already proves who
	// this is. A session can be a borrowed laptop; knowing the old password is
	// what makes this the account holder rather than whoever sat down at it.
	_, hash, err := s.store.CredentialsFor(r.Context(), user.Email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		// Rate limited like a login, because that is exactly what this is: an
		// online guess against a bcrypt hash.
		key := "pwchange|" + clientIP(r) + "|" + store.NormalizeEmail(user.Email)
		retryIn, _ := s.store.RateRetryIn(r.Context(), key)
		if retryIn > 0 {
			s.renderPasswordForm(w, r, http.StatusTooManyRequests,
				"Too many failed attempts. Try again "+retryPhrase(retryIn)+".")
			return
		}
		s.store.RateFail(r.Context(), key)
		s.renderPasswordForm(w, r, http.StatusUnauthorized,
			"That is not your current password.")
		return
	}

	if msg := validateNewPassword(password, confirm); msg != "" {
		s.renderPasswordForm(w, r, http.StatusBadRequest, msg)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil {
		s.renderPasswordForm(w, r, http.StatusBadRequest,
			"That is the password you already have. Choose a different one.")
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Keeping this session and dropping the others: signing someone out of the
	// page they are looking at, moments after they proved they know the password,
	// would be gratuitous. Every other device goes, which is the half that
	// matters.
	if err := s.store.ChangePassword(r.Context(), user.ID, string(newHash), currentSession(r)); err != nil {
		s.serverError(w, r, err)
		return
	}

	log.Printf("auth: user %d changed their password; other sessions revoked", user.ID)
	s.flashSuccess(w, r, "Password changed. Any other device has been signed out.")
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}

// conflictOnUpdate re-renders the edit form after a losing race.
//
// It shows what the other person saved next to what this user typed, and carries
// the new version forward so that saving again succeeds. That last part matters:
// a conflict page that cannot be resolved by pressing the button again is just a
// dead end with extra words.
func (s *Server) conflictOnUpdate(w http.ResponseWriter, r *http.Request,
	sc store.Scope, id int64, in transactionInput) {

	current, err := s.store.ByID(r.Context(), sc, id)
	if err != nil {
		// It was there a moment ago -- the row has since gone entirely.
		http.NotFound(w, r)
		return
	}

	msg := fmt.Sprintf(
		"Somebody else changed this entry while you had it open. "+
			"It now reads %s — %s on %s%s. "+
			"Your version is still in the form below; save again to replace theirs.",
		current.Label, current.Amount.Display(), current.OccurredOn,
		addedByPhrase(current.AddedBy))

	s.rerenderTransactionFormAt(w, r, in, msg, true, id, current.Version)
}

func addedByPhrase(who string) string {
	if strings.TrimSpace(who) == "" {
		return ""
	}
	return ", entered by " + who
}
