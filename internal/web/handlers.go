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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/insight"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/ocr"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// landingView backs the landing page, which is both the login form and the signup
// form: an address with no account is offered one on a second state of the same page,
// which is what Confirming switches on. There is no separate /register route.
type landingView struct {
	view

	Error string
	Email string

	// Confirming is true when the address was not recognised and the user is
	// being asked whether to create an account.
	Confirming bool

	// NewAccount is set by ?new=1 from the "Sign up" link.
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
	if !s.parseForm(w, r) {
		return
	}

	// The landing form is unauthenticated but still carries a token, minted when the page
	// was rendered.
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

	// The address is checked for shape before it is looked up, so a value that is not an
	// email is rejected whether or not an account matches it.
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

	limit := newLoginLimit(r, email)
	retryIn, err := s.retryIn(r.Context(), limit)
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
		s.attemptLogin(w, r, email, password, limit)
		return
	}

	// Unknown address. First submission asks; second submission creates.
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
		s.attemptLogin(w, r, email, password, limit)
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
	s.redirectSuccess(w, r, "/dashboard", "Welcome to YABA. Add some income to get started.")
}

// loginLimit is the pair of counters guarding one password attempt.
//
// The account counter is keyed on address and IP together: on IP alone one
// attacker could lock out a shared network, on the address alone anyone could
// lock a known user out of their own account. But that key changes with every
// address tried, so on its own it stops nobody from working through a list of
// addresses at ten guesses each from a single machine. The ip counter is what
// closes that, at a budget high enough (store.RateBurstTries) that ordinary
// mistyping on a shared network never reaches it.
type loginLimit struct {
	account string
	ip      string
}

func newLoginLimit(r *http.Request, email string) loginLimit {
	ip := clientIP(r)
	return loginLimit{
		account: ip + "|" + store.NormalizeEmail(email),
		ip:      "ip|" + ip,
	}
}

// retryIn reports how long before another password may be tried, zero if now.
func (s *Server) retryIn(ctx context.Context, l loginLimit) (time.Duration, error) {
	wait, err := s.store.RateRetryIn(ctx, l.account)
	if err != nil || wait > 0 {
		return wait, err
	}
	return s.store.RateRetryInMax(ctx, l.ip, store.RateBurstTries)
}

// rateFail charges a failure to both counters. Only the account counter is
// cleared on a successful sign-in: an attacker who owns one account on the box
// would otherwise reset the spray counter at will, simply by logging in.
func (s *Server) rateFail(ctx context.Context, l loginLimit) {
	s.store.RateFail(ctx, l.account)
	s.store.RateFail(ctx, l.ip)
}

// attemptLogin verifies a password for a known address.
func (s *Server) attemptLogin(w http.ResponseWriter, r *http.Request, email, password string, limit loginLimit) {
	user, hash, err := s.store.CredentialsFor(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		// Compare against a dummy hash anyway, so an unknown address takes about as long as a
		// known one.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		s.rateFail(r.Context(), limit)
		s.renderLanding(w, r, http.StatusUnauthorized, landingView{
			Email: email, Error: "That email and password do not match."})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.rateFail(r.Context(), limit)
		s.renderLanding(w, r, http.StatusUnauthorized, landingView{
			Email: email, Error: "That email and password do not match."})
		return
	}

	s.store.RateReset(r.Context(), limit.account)
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	log.Printf("auth: login ok user=%d", user.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// startSession issues a fresh session for a newly authenticated user, so the identifier
// rotates on privilege change and a token planted before login is useless after it.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	sid, err := s.store.CreateSession(r.Context(), userID, r.UserAgent())
	if err != nil {
		return err
	}

	// Housekeeping, on a path that is already doing a write and is not latency-sensitive.
	if n, err := s.store.PurgeExpiredSessions(r.Context()); err != nil {
		log.Printf("auth: purge expired sessions: %v", err)
	} else if n > 0 {
		log.Printf("auth: purged %d expired session(s)", n)
	}

	// Get, not New. Get registers the session for this request, so a flash set
	// later in the same handler lands on this session and is saved with the sid.
	// With New it was a second, unregistered session: the flash re-saved the
	// pre-login cookie and its Set-Cookie came last, so a browser kept a session
	// with no sid and every new account bounced straight back to the login page.
	// Every pre-login value is cleared, which is the rotation New provided.
	session, _ := s.sessions.Get(r, sessionName)
	for k := range session.Values {
		delete(session.Values, k)
	}
	session.Values[sessionID] = sid
	if tok, err := randomToken(32); err == nil {
		session.Values[sessionCSRFToken] = tok
	} else {
		log.Printf("auth: could not mint CSRF token: %v", err)
	}
	if err := session.Save(r, w); err != nil {
		// The row exists but the browser never got its token, so nothing can present it.
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

	// Delete the row, not just the cookie. Clearing the cookie leaves a live session that
	// anyone holding a copy of the token could keep using: signing out has to destroy the
	// credential, not merely forget it locally.
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

	if !s.parseForm(w, r) {
		return
	}
	target := r.PostFormValue("session_id")

	// Revoking the session you are using is just a logout, and saying so is
	// kinder than silently ending the request with a redirect that fails auth.
	if target == currentSession(r) {
		s.redirectError(w, r, "/sessions", "That is this device. Use Log out instead.")
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

// handleForgot backs the Forgot password link. It does not claim to have sent what it
// could not send: a page saying check your inbox while no mail server is configured
// leaves the user waiting for a message that never arrives.
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

// issueFormToken mints a one-time token for a form that creates something.
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

// retryPhrase turns a remaining lockout into words, rounded up to whole minutes so the
// time it states actually works.
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

// validateEmail checks the address is plausibly an email address.
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

// validateNewPassword is validatePassword plus a confirmation field, for wherever the
// user cannot immediately test the password by signing in.
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
	SpendTotal money.Cents

	// The needs/wants split, shown under the expenses total.
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

	month := parseMonth(r.URL.Query().Get("month"))

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

	if err := s.loadDashboard(ctx, sc, month, budgetMonth, &v); err != nil {
		s.serverError(w, r, err)
		return
	}

	v.Charts = buildDashboardCharts(v)
	s.render(w, r, "dashboard.html", v)
}

// loadDashboard fills the view, one card at a time and in order: the emergency
// fund's runway is measured against the buckets the expenses card reads, so it
// cannot run before it. Straight-line calls rather than a list of loaders,
// because that ordering is real and a slice would hide it.
func (s *Server) loadDashboard(ctx context.Context, sc store.Scope, month, budgetMonth string, v *dashboardView) error {
	var err error
	if v.Months, err = s.store.Months(ctx, sc); err != nil {
		return err
	}
	if err := s.loadCurrentFunds(ctx, sc, v); err != nil {
		return err
	}
	if err := s.loadIncome(ctx, sc, month, v); err != nil {
		return err
	}
	if err := s.loadExpenses(ctx, sc, month, budgetMonth, v); err != nil {
		return err
	}
	if err := s.loadEmergencyFund(ctx, sc, v); err != nil {
		return err
	}
	if err := s.loadSavingsGrid(ctx, sc, v); err != nil {
		return err
	}
	v.PendingReceipts, err = s.store.PendingReceiptCount(ctx, sc)
	return err
}

// loadCurrentFunds fills card 1. Cash is a balance, not a flow, so it is
// all-time however the period filter is set: scoping a balance to one month
// would simply be wrong.
func (s *Server) loadCurrentFunds(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.Cash, err = s.store.Cash(ctx, sc); err != nil {
		return err
	}
	if v.Balance, err = s.store.BalanceSeries(ctx, sc); err != nil {
		return err
	}
	v.Trend = insight.FitTrend(v.Balance)
	return nil
}

// loadIncome fills card 3, plus the actual totals for the selected period.
func (s *Server) loadIncome(ctx context.Context, sc store.Scope, month string, v *dashboardView) error {
	var err error
	if v.Monthly, err = s.store.MonthlySeries(ctx, sc, 12); err != nil {
		return err
	}
	v.IncomeRange = insight.EstimateMonthlyIncome(v.Monthly)
	if v.IncomeBySource, err = s.store.Breakdown(ctx, sc, store.KindIncome, month); err != nil {
		return err
	}

	periodTotals, err := s.store.Totals(ctx, sc, month)
	if err != nil {
		return err
	}
	v.IncomeTotal, v.SpendTotal = periodTotals.Income, periodTotals.Expense

	v.Essential, v.NonEssent, err = s.store.EssentialSplit(ctx, sc, month)
	return err
}

// loadExpenses fills card 4. Buckets is the most expensive read on the page and
// is taken exactly once here: everything else that needs it -- the estimate, the
// allocation summary, and the emergency fund's essential cost -- is handed the
// slice rather than querying again.
func (s *Server) loadExpenses(ctx context.Context, sc store.Scope, month, budgetMonth string, v *dashboardView) error {
	var err error
	if v.Buckets, err = s.store.Buckets(ctx, sc, budgetMonth); err != nil {
		return err
	}
	v.ExpenseRange = insight.EstimateMonthlyExpenses(v.Buckets)
	// CategoryBreakdown rather than Breakdown, so a split transaction reports
	// each of its line items under its own category.
	if v.SpendByCategory, err = s.store.CategoryBreakdown(ctx, sc, month); err != nil {
		return err
	}
	v.Allocation, err = s.store.AllocationsFor(ctx, sc, budgetMonth, v.Buckets)
	return err
}

// loadEmergencyFund fills card 2. Runs after loadExpenses, whose buckets decide
// how much a month of essentials costs and therefore how long the fund lasts.
func (s *Server) loadEmergencyFund(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.EmergencyFund, err = s.store.EmergencyFund(ctx, sc); err != nil {
		return err
	}
	withdrawals, err := s.store.FundWithdrawalHistory(ctx, sc, v.EmergencyFund.ID)
	if err != nil {
		return err
	}
	v.Runway = insight.AssessEmergencyFund(v.EmergencyFund, withdrawals, store.EssentialCost(v.Buckets))
	return nil
}

// loadSavingsGrid fills every fund with its own projection, for the grid on the
// savings tab.
func (s *Server) loadSavingsGrid(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.Totals, err = s.store.Totals(ctx, sc, ""); err != nil {
		return err
	}
	funds, err := s.store.ListFunds(ctx, sc)
	if err != nil {
		return err
	}
	rates, err := s.store.DepositRates(ctx, sc)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, f := range funds {
		rate := rates[f.ID]
		v.Funds = append(v.Funds, fundCard{
			Fund:       f,
			Projection: insight.ProjectFund(f, insight.AverageMonthlyDeposit(rate.Total, rate.Months), now),
		})
	}
	return nil
}

func buildDashboardCharts(v dashboardView) chartData {
	c := chartData{BalanceLabels: []string{}, BalanceValues: []float64{}, TrendValues: []float64{}}
	for _, p := range v.Balance {
		c.BalanceLabels = append(c.BalanceLabels, p.Date)
		c.BalanceValues = append(c.BalanceValues, p.Balance.Float())
	}
	if v.Trend.OK {
		c.TrendValues = v.Trend.Values
	}
	c.MonthLabels, c.MonthIncome, c.MonthExpense = monthAxis(v.Monthly)
	// Blank labels are grouped rather than drawn as an unnamed slice.
	c.IncomeLabels, c.IncomeValues = labelAxis(v.IncomeBySource, "Unlabelled")
	c.SpendLabels, c.SpendValues = labelAxis(v.SpendByCategory, "Unlabelled")
	return c
}

// monthAxis and labelAxis turn a series into the parallel arrays Chart.js wants.
// The slices are never nil: encoding/json renders nil as null, which makes
// Chart.js throw, whereas an empty array draws nothing.
func monthAxis(months []store.MonthPoint) (labels []string, income, expense []float64) {
	labels, income, expense = []string{}, []float64{}, []float64{}
	for _, m := range months {
		label := m.Month
		if t, err := time.Parse(store.MonthLayout, m.Month); err == nil {
			label = t.Format("Jan 06")
		}
		labels = append(labels, label)
		income = append(income, m.Income.Float())
		expense = append(expense, m.Expense.Float())
	}
	return labels, income, expense
}

func labelAxis(totals []store.LabelTotal, blank string) (labels []string, values []float64) {
	labels, values = []string{}, []float64{}
	for _, lt := range totals {
		label := lt.Label
		if label == "" {
			label = blank
		}
		labels = append(labels, label)
		values = append(values, lt.Total.Float())
	}
	return labels, values
}

// fundCard pairs a savings fund with its projection.
type fundCard struct {
	store.Fund
	Projection insight.Projection
}

// handleSavingsRedirect keeps the old /savings path working, sending a bookmark to the
// tab that now does the job rather than 404ing it.
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

	month := parseMonth(r.URL.Query().Get("month"))

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
	alloc, err := s.store.AllocationsFor(ctx, sc, store.Today()[:7], buckets)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ef, err := s.store.EmergencyFund(ctx, sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	wd, err := s.store.FundWithdrawalHistory(ctx, sc, ef.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	runway := insight.AssessEmergencyFund(ef, wd, store.EssentialCost(buckets))

	v.Observations = insight.Observations(v.Totals, v.Essential, v.NonEssent, v.Budgets, v.Monthly)
	v.Observations = append(v.Observations, bucketObservations(alloc, buckets, runway)...)

	v.Charts = buildReportCharts(v)
	s.render(w, r, "reports.html", v)
}

func buildReportCharts(v reportsView) reportCharts {
	c := reportCharts{EssentialLabels: []string{}, EssentialValues: []float64{}}
	c.IncomeLabels, c.IncomeValues = labelAxis(v.IncomeBySource, "")
	c.SpendLabels, c.SpendValues = labelAxis(v.SpendByCategory, "")
	c.MonthLabels, c.MonthIncome, c.MonthExpense = monthAxis(v.Monthly)
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

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	f := listFilter(q)
	f.Limit = store.DefaultPageSize
	f.Offset = (page - 1) * store.DefaultPageSize

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
			Value: k.v, Label: k.l, Selected: k.v == string(f.Kind),
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

// formFields is the state of an income or expense form: the wireframe's five Ws
// (What, When, Who, Where, Why) plus amount, essential flag and recurring
// bucket. Shared by the dedicated entry pages and the add/edit form, and echoed
// back after a validation failure so nothing typed is lost.
type formFields struct {
	Label      string // What?
	Amount     string
	OccurredOn string // When?
	Essential  bool
	Payee      string // Who?
	Place      string // Where?
	Note       string // Why?
	BucketID   int64
}

// echoForm reads the submitted values back for re-rendering.
func echoForm(r *http.Request, in transactionInput) formFields {
	f := formFields{
		Label:      r.PostFormValue("label"),
		Amount:     r.PostFormValue("amount"),
		OccurredOn: r.PostFormValue("date"),
		Essential:  r.PostFormValue("essential") != "no",
		Payee:      r.PostFormValue("payee"),
		Place:      r.PostFormValue("place"),
		Note:       r.PostFormValue("note"),
	}
	if in.bucketID != nil {
		f.BucketID = *in.bucketID
	}
	if f.OccurredOn == "" {
		f.OccurredOn = store.Today()
	}
	return f
}

// transactionFormView backs the shared add/edit form.
type transactionFormView struct {
	view

	// Editing is false for a new entry.
	Editing bool
	ID      int64
	Action  string

	Kind store.Kind
	formFields
	Buckets []store.Bucket

	// Items backs the optional line-item breakdown.
	Items []store.LineItem

	Categories []string
	Error      string

	// FormToken is a one-time token for the create path; empty when editing,
	// because an edit is already protected by the version check.
	FormToken string

	// ReceiptJobID is a receipt already uploaded and waiting to be described, carried in a
	// hidden field so it survives a validation error: otherwise correcting a typo would
	// silently detach the receipt the user came here to attach.
	ReceiptJobID   int64
	ReceiptName    string
	ReceiptMissing bool

	// Draft is what OCR read off that receipt, or nil. The template uses it to
	// say where the prefilled figures came from and how sure the reading was, so
	// the user knows which fields to check rather than trusting the form.
	Draft *store.ReceiptDraft

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
		formFields: formFields{OccurredOn: store.Today(), Essential: true},
		Action:     "/transactions/new",
	}
	// Only the create path needs one. An edit cannot duplicate anything: it
	// carries a version, and the second save of the same version is refused.
	if r.PathValue("id") == "" {
		v.FormToken = s.issueFormToken(r, "transaction")
	}

	// A receipt uploaded earlier that could not be read automatically, whose notification
	// sent the user here to finish it by hand.
	if raw := r.URL.Query().Get("receipt"); raw != "" && kind == store.KindExpense {
		jobID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Either it belongs to another budget, or somebody already turned it into an
			// expense.
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
			// Everything OCR read. Each field is only filled in when it was
			// actually recovered, so a partial reading prefills what it found and
			// leaves the rest blank rather than inventing a plausible default.
			if d := job.Draft; d != nil {
				v.Draft = d
				if d.Total > 0 {
					v.Amount = d.Total.Input()
				}
				if d.Date != "" {
					v.OccurredOn = d.Date
				}
				if d.Merchant != "" {
					v.Payee = d.Merchant
				}
				if d.Category != "" {
					// The guessed category beats the filename guess above: it
					// came from the receipt's contents rather than from whatever
					// the camera happened to name the file.
					v.Label = d.Category
				}
				for i, it := range d.Items {
					v.Items = append(v.Items, store.LineItem{
						Description: it.Description,
						Amount:      it.Amount,
						Position:    i,
					})
				}
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
			s.redirectError(w, r, "/transactions", "Savings transfers can't be edited. Withdraw from the fund instead.")
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

	// version is the row version the form was rendered from.
	version int64
}

// parseTransactionForm validates every field and returns a message fit to show the user,
// rather than redirecting to an empty form with the input discarded.
func parseTransactionForm(r *http.Request) (transactionInput, string) {
	kind := store.Kind(r.PostFormValue("kind"))
	if kind != store.KindIncome && kind != store.KindExpense {
		return transactionInput{}, "Choose whether this is income or an expense."
	}
	return parseTransactionFormFor(r, kind)
}

// parseTransactionFormFor validates with the kind imposed by the caller rather than read
// from the request, which is what makes /income and /expense genuinely separate: a
// hand-edited POST cannot store an expense as income.
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

	// The version the editor was shown, used as a compare-and-swap on save.
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
	// Requiring the lines to reconcile is what keeps the category breakdown and the
	// headline total telling the same story.
	if sum != total {
		return nil, fmt.Sprintf("The line items add up to %s but the total is %s.",
			sum.Display(), total.Display())
	}
	return items, ""
}

func (s *Server) handleTransactionCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, false, 0)
		return
	}

	if s.duplicateSubmit(r, user.ID) {
		s.redirectSuccess(w, r, "/dashboard", "That entry was already saved.")
		return
	}

	n := in.toNewTransaction()

	// An expense may arrive with a receipt attached in the same submission.
	if in.kind == store.KindExpense {
		path, name, err := s.saveReceipt(r, user.ID)
		if err != nil {
			s.rerenderTransactionForm(w, r, in, err.Error(), false, 0)
			return
		}
		n.ReceiptPath, n.ReceiptName = path, name
	}

	// Or it may be finishing a receipt uploaded earlier: validated before the insert, so a
	// stolen or already-used id fails without leaving a stray expense behind.
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

	ws := s.finishSave(r, sc, txID, in, in.occurredOn)
	if attachJobID != 0 {
		if err := s.store.AttachReceipt(r.Context(), sc, attachJobID, txID); err != nil {
			// The expense is saved either way, so this reports rather than fails.
			ws.add("The receipt could not be attached to it.")
		}
	}
	s.redirectSaved(w, r, "/dashboard", ws,
		in.kind.Label()+" of "+in.amount.Display()+" saved.")
}

// warnings are the things that went wrong after the row was already written.
//
// They are collected rather than flashed as they happen because there is one
// flash slot per redirect: a warning set here would be overwritten by the
// success message the handler sets a moment later, and the user would be told
// everything worked while the line items, the receipt or the month's funding
// were quietly missing.
type warnings []string

func (ws *warnings) add(format string, a ...any) {
	*ws = append(*ws, fmt.Sprintf(format, a...))
}

// fold combines the warnings with the message the handler wanted to show. A
// partial failure is reported as an error, because it is one.
func (ws warnings) fold(success string) (kind, text string) {
	if len(ws) == 0 {
		return "success", success
	}
	return "error", success + " " + strings.Join(ws, " ")
}

// redirectSaved sends the user on with a single message covering both what was
// saved and anything that did not survive the save.
func (s *Server) redirectSaved(w http.ResponseWriter, r *http.Request, to string, ws warnings, success string) {
	kind, text := ws.fold(success)
	s.setFlash(w, r, kind, text)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// finishSave stores the optional line-item breakdown and re-pours the funding
// waterfall: income funds the priority list, and a bucket-attributed expense
// changes what a variable bucket costs. The transaction is already saved, so a
// failure here is reported alongside the success rather than instead of it.
//
// Every month named is recalculated. An edit that moves an entry passes two --
// the month it left and the month it joined -- because the waterfall is stored
// per month and re-pouring only the new one leaves the old holding allocations
// against money that is no longer in it.
func (s *Server) finishSave(r *http.Request, sc store.Scope, txID int64, in transactionInput, months ...string) warnings {
	var ws warnings
	if len(in.items) > 0 {
		if err := s.store.SetLineItems(r.Context(), sc, txID, in.items); err != nil {
			ws.add("The line items could not be stored: %s", err)
		}
	}
	ws.addReallocations(r, s.store, sc, months...)
	return ws
}

// addReallocations re-pours each distinct month named, ignoring blanks.
func (ws *warnings) addReallocations(r *http.Request, st *store.Store, sc store.Scope, months ...string) {
	done := map[string]bool{}
	for _, m := range months {
		if len(m) < 7 || done[m[:7]] {
			continue
		}
		done[m[:7]] = true
		if err := st.ReallocateMonthOf(r.Context(), sc, m); err != nil {
			ws.add("The expense funding for %s could not be recalculated.", m[:7])
		}
	}
}

func (s *Server) handleTransactionUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, true, id)
		return
	}

	// The month the entry is leaving, read before the write. An edit can move an
	// entry between months and the funding waterfall is stored per month, so
	// both have to be re-poured -- handleTransactionDelete reads the date first
	// for the same reason.
	wasOn := ""
	if t, e := s.store.ByID(r.Context(), sc, id); e == nil {
		wasOn = t.OccurredOn
	}

	err := s.store.Update(r.Context(), sc, id, in.toNewTransaction())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		// Somebody else saved this row while the form was open.
		s.conflictOnUpdate(w, r, sc, id, in)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Passing an empty slice clears the breakdown, which is how a user removes
	// line items: they blank the rows and save.
	var ws warnings
	if err := s.store.SetLineItems(r.Context(), sc, id, in.items); err != nil {
		ws.add("The line items could not be stored: %s", err)
	}
	ws.addReallocations(r, s.store, sc, wasOn, in.occurredOn)

	s.redirectSaved(w, r, "/transactions", ws, "Entry updated.")
}

func (s *Server) handleTransactionDelete(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// Read the date before deleting, so the right month can be recalculated.
	month := ""
	if t, e := s.store.ByID(r.Context(), sc, id); e == nil {
		month = t.OccurredOn
	}

	err := s.store.Delete(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		// Covers both no such row and belongs to another household.
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
		formFields: echoForm(r, in),
		Error:      msg,
		Action:     "/transactions/new",
		Version:    version,
	}
	// Echo the submitted line items back so a validation failure does not throw
	// away rows the user typed.
	for i, it := range in.items {
		v.Items = append(v.Items, store.LineItem{
			Description: it.Description, Category: it.Category,
			Amount: it.Amount, Position: i,
		})
	}
	// A fresh token, not the one that was posted. Form validation does fail
	// before the token is spent, but three later refusals do not: an unusable
	// receipt, a receipt id already attached to something else, and a bucket
	// deleted since the page was opened. Echoing the spent token back meant the
	// user's corrected resubmit was answered with "That entry was already
	// saved." while recording nothing -- losing the entry and denying it in the
	// same breath. A new token per rendered page is the invariant that stops a
	// double submit anyway; reusing one was never what made it work.
	if !editing {
		v.FormToken = s.issueFormToken(r, "transaction")
	}

	// Keep the pending receipt across a validation failure, along with what OCR
	// read from it: losing the image and the reading because a date was mistyped
	// would send the user back to find the receipt again.
	if raw := r.PostFormValue("receipt_job"); raw != "" {
		if jobID, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID); err == nil {
				v.ReceiptJobID = job.ID
				v.ReceiptName = job.OriginalName
				v.Draft = job.Draft
			}
		}
	}

	if editing {
		v.Action = fmt.Sprintf("/transactions/%d/edit", id)
		v.Title = "Edit entry"
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
	month := parseMonth(q.Get("month"))

	txs, err := s.store.All(r.Context(), sc, listFilter(q))
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

	// "Added by" is always a column here, unlike in the on-screen table where it is hidden
	// for a solo user.
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
		// Amounts are written as plain decimals with no currency symbol or thousands
		// separator, because a spreadsheet will not parse "$1,234.56" as a number.
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

// csvSafe neutralises spreadsheet formula injection: a label like =1+1 or @SUM(A1:A9)
// is executed as a formula when the file is opened in Excel or Sheets.
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
		// Income has no essential flag and no recurring-expense bucket.
		n.BucketID = nil
	}
	return n
}

// ═════════════════════════════════════════════════════════════════════════════
// entry.go
// ═════════════════════════════════════════════════════════════════════════════

// entryView backs the two dedicated entry pages, /income and /expense.
type entryView struct {
	view

	// Kind is what this page adds. The template does not offer a choice.
	Kind store.Kind

	formFields
	Error string

	// Step is "choose" or "manual" on the expense page.
	Step string

	// Suggestions for the label field, and buckets an expense can pay towards, and
	// deliberately nothing else: the charts and recent lists live on the dashboard, so the
	// queries that fed them here are gone rather than run and discarded.
	Categories []string
	Buckets    []store.Bucket

	// FormToken is a one-time token, so a double click or a back-then-save does
	// not record the same money twice.
	FormToken string

	// Waiting are receipts already processed that nobody has turned into an expense yet.
	Waiting []store.ReceiptJob
}

// handleIncomePage is GET /income.
func (s *Server) handleIncomePage(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ProcessDueRecurringIncome(
		r.Context(),
		scopeOf(r),
		store.Today(),
	); err != nil {
		http.Error(w, "Could not process recurring income.", http.StatusInternalServerError)
		return
	}

	v, ok := s.buildEntryView(w, r, store.KindIncome, "Add Income", "income")
	if !ok {
		return
	}

	s.render(w, r, "income.html", v)
}

// handleExpensePage is GET /expense. It opens on the Manual-or-Upload chooser rather
// than a form: the user picks how to add the expense before seeing any fields.
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

// buildEntryView loads everything both pages need.
func (s *Server) buildEntryView(w http.ResponseWriter, r *http.Request, kind store.Kind, title, nav string) (entryView, bool) {
	sc := scopeOf(r)
	ctx := r.Context()

	v := entryView{
		view:       s.baseView(w, r, title, nav),
		Kind:       kind,
		formFields: formFields{OccurredOn: store.Today(), Essential: true},
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
	if !s.parseForm(w, r) {
		return
	}

	// If this is not a recurring-income submission, use the normal
	// one-time income flow.
	if r.PostFormValue("income_type") != "recurring" {
		s.createEntry(w, r, store.KindIncome)
		return
	}

	user := mustUser(r)
	sc := scopeOf(r)

	in, msg := parseTransactionFormFor(r, store.KindIncome)
	if msg != "" {
		s.rerenderEntry(w, r, store.KindIncome, in, msg)
		return
	}

	frequencyN, err := strconv.Atoi(r.PostFormValue("frequency_n"))
	if err != nil || frequencyN <= 0 {
		s.rerenderEntry(
			w,
			r,
			store.KindIncome,
			in,
			"Enter a valid recurring frequency.",
		)
		return
	}

	frequencyUnit := r.PostFormValue("frequency_unit")
	switch frequencyUnit {
	case "day", "week", "month":
	default:
		s.rerenderEntry(
			w,
			r,
			store.KindIncome,
			in,
			"Choose a valid recurring frequency.",
		)
		return
	}

	if s.duplicateSubmit(r, user.ID) {
		s.redirectSuccess(w, r, "/income", "That recurring income was already saved.")
		return
	}

	_, err = s.store.CreateRecurringIncome(
		r.Context(),
		sc,
		in.label,
		in.amount,
		frequencyN,
		frequencyUnit,
		in.occurredOn,
	)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(
		w,
		r,
		"/income",
		fmt.Sprintf("Recurring income of %s saved.", in.amount.Display()),
	)
}

// handleExpenseCreate is POST /expense.
func (s *Server) handleExpenseCreate(w http.ResponseWriter, r *http.Request) {
	s.createEntry(w, r, store.KindExpense)
}

// createEntry saves one entry of a fixed kind. The kind comes from the route, never from
// the form, so a hand-edited request cannot post an expense to the income page.
func (s *Server) createEntry(w http.ResponseWriter, r *http.Request, kind store.Kind) {
	user := mustUser(r)
	sc := scopeOf(r)

	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionFormFor(r, kind)
	if msg != "" {
		s.rerenderEntry(w, r, kind, in, msg)
		return
	}

	// Validated, so this is a real submission -- spend the token.
	if s.duplicateSubmit(r, user.ID) {
		s.redirectSuccess(w, r, "/dashboard", "That entry was already saved.")
		return
	}

	n := in.toNewTransaction()

	// An expense may carry a receipt in the same submission.
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

	ws := s.finishSave(r, sc, txID, in, in.occurredOn)

	// Back to the same page, so another entry can be added without navigating.
	back := "/expense?step=manual"
	if kind == store.KindIncome {
		back = "/income"
	}
	s.redirectSaved(w, r, back, ws, kind.Label()+" of "+in.amount.Display()+" saved.")
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
	v.formFields = echoForm(r, in)
	if kind == store.KindExpense {
		v.Step = "manual"
	}

	s.renderStatus(w, r, http.StatusBadRequest, page, v)
}

// ═════════════════════════════════════════════════════════════════════════════
// funds.go
// ═════════════════════════════════════════════════════════════════════════════

// Fund handlers redirect back to the Emergency Fund tab.
const savingsAnchor = "/dashboard?tab=emergency"

func (s *Server) handleFundCreate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirectError(w, r, savingsAnchor, "Give the fund a name.")
		return
	}

	// A goal is optional, so an empty field is zero rather than an error.
	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.redirectError(w, r, savingsAnchor, msg)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.redirectError(w, r, savingsAnchor, msg)
		return
	}

	if _, err := s.store.CreateFund(r.Context(), sc, name, goal, months); err != nil {
		s.redirectError(w, r, savingsAnchor, err.Error())
		return
	}

	s.redirectSuccess(w, r, savingsAnchor, fmt.Sprintf("Fund %q created.", name))
}

func (s *Server) handleFundGoal(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.redirectError(w, r, savingsAnchor, msg)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.redirectError(w, r, savingsAnchor, msg)
		return
	}

	// A rename can arrive on the same form; an empty field leaves it alone.
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
		if err := s.store.RenameFund(r.Context(), sc, fundID, name); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			s.redirectError(w, r, savingsAnchor, err.Error())
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
	s.moveFund(w, r, true)
}

func (s *Server) handleFundWithdraw(w http.ResponseWriter, r *http.Request) {
	s.moveFund(w, r, false)
}

// moveFund is the shared body of deposit and withdrawal. Note what is NOT read
// from the form: the fund's balance. The store re-derives it inside the
// transaction, which is what makes an over-withdrawal impossible.
func (s *Server) moveFund(w http.ResponseWriter, r *http.Request, deposit bool) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok || !s.parseForm(w, r) {
		return
	}

	direction := "to take out of the fund"
	if deposit {
		direction = "to move into the fund"
	}
	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		s.redirectError(w, r, savingsAnchor, "Enter an amount greater than zero "+direction+".")
		return
	}
	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		s.redirectError(w, r, savingsAnchor, "Enter a date in YYYY-MM-DD form.")
		return
	}

	var short error
	var shortText, done string
	if deposit {
		err = s.store.Deposit(r.Context(), sc, fundID, amount, date)
		short, shortText = store.ErrInsufficientCash, "Not enough available cash. "
		done = fmt.Sprintf("%s moved into savings.", amount.Display())
	} else {
		err = s.store.Withdraw(r.Context(), sc, fundID, amount, date)
		short, shortText = store.ErrInsufficientFund, "Not enough in that fund. "
		done = fmt.Sprintf("%s returned to available cash.", amount.Display())
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case errors.Is(err, short):
		// The store's message already names the available balance.
		s.flashError(w, r, shortText+trimSentinel(err, short))
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, done)
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
	if !s.parseForm(w, r) {
		return
	}

	category := strings.TrimSpace(r.PostFormValue("category"))
	if category == "" {
		s.redirectError(w, r, "/dashboard#budgets", "Choose a category to budget.")
		return
	}

	limit, err := money.ParsePositive(r.PostFormValue("limit"))
	if err != nil {
		s.redirectError(w, r, "/dashboard#budgets", "Enter a monthly limit greater than zero.")
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

// parseForm reads the body, answering 400 if it cannot be read.
func (s *Server) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, "Could not read that form.")
		return false
	}
	return true
}

// redirectError flashes a failure and sends the user back to the page the
// action lives on. redirectSuccess is the same for a success.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, to, msg string) {
	s.flashError(w, r, msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) redirectSuccess(w http.ResponseWriter, r *http.Request, to, msg string) {
	s.flashSuccess(w, r, msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// parseMonth validates a ?month= value. Anything unparseable means all time
// rather than an error, so a hand-edited URL degrades instead of failing.
func parseMonth(raw string) string {
	if _, err := time.Parse(store.MonthLayout, raw); err != nil {
		return ""
	}
	return raw
}

// listFilter reads the type, month and search filters shared by the
// transaction list and its CSV export. An unknown type shows everything rather
// than erroring, deliberately.
func listFilter(q url.Values) store.Filter {
	kind := store.Kind(q.Get("type"))
	if !kind.Valid() {
		kind = ""
	}
	return store.Filter{
		Kind:   kind,
		Month:  parseMonth(q.Get("month")),
		Search: q.Get("q"),
	}
}

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

// pathIDOrBadRequest is pathID for the household routes, which answer 400 rather
// than 404 to a malformed id and say what kind of id was expected.
func (s *Server) pathIDOrBadRequest(w http.ResponseWriter, r *http.Request, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not "+what+".")
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
	if !s.parseForm(w, r) {
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.redirectError(w, r, expensesTab, msg)
		return
	}

	if _, err := s.store.CreateBucket(r.Context(), sc, n); err != nil {
		s.redirectError(w, r, expensesTab, err.Error())
		return
	}

	// A new expense changes what the month requires, so the waterfall is re-poured.
	s.reallocateNow(w, r)

	s.redirectSuccess(w, r, expensesTab, fmt.Sprintf("%q added to your recurring expenses.", n.Name))
}

func (s *Server) handleBucketUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.redirectError(w, r, expensesTab, msg)
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

// moveBucket reorders the priority list. Not cosmetic: priority decides which expense
// is funded first when income arrives, so a move has to re-run the allocation.
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
func (s *Server) handleReallocate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	month := strings.TrimSpace(r.PostFormValue("month"))

	if err := s.store.Reallocate(r.Context(), sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectSuccess(w, r, expensesTab, "Income re-allocated down the priority list.")
}

// reallocateNow re-pours the waterfall for the current month and reports a failure
// rather than leaving the page silently stale.
func (s *Server) reallocateNow(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Reallocate(r.Context(), scopeOf(r), ""); err != nil {
		// Not fatal: the change the user asked for did happen.
		s.flashError(w, r, "Saved, but the expense funding could not be recalculated. Try the Recalculate button.")
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// receipts.go
// ═════════════════════════════════════════════════════════════════════════════

// allowedReceiptKinds are the formats a receipt may be uploaded in.
//
// The types are decided by ocr.Sniff rather than http.DetectContentType, which
// cannot recognise HEIC at all: it reports it as application/octet-stream, so
// the old allowlist silently rejected the format every recent iPhone
// photographs in by default -- which is to say, the single most likely thing
// somebody would try to upload a receipt as.
var allowedReceiptKinds = map[ocr.Kind]bool{
	ocr.KindJPEG: true,
	ocr.KindPNG:  true,
	ocr.KindWebP: true,
	ocr.KindGIF:  true,
	ocr.KindHEIC: true,
	ocr.KindAVIF: true,
	ocr.KindPDF:  true,
}

func (s *Server) maxUploadBytes() int64 {
	mb := s.cfg.MaxUploadMB
	if mb <= 0 {
		mb = 5
	}
	return mb << 20
}

// saveReceipt stores an uploaded receipt and returns its path and original name.
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

	kind := ocr.Sniff(head)
	if !allowedReceiptKinds[kind] {
		return "", "", fmt.Errorf(
			"receipts must be a photo or a PDF — JPEG, PNG, HEIC, WebP, GIF or PDF")
	}
	ext := kind.Ext()

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

	// O_EXCL means a name collision fails rather than overwriting an existing receipt.
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
	return filepath.ToSlash(full), cleanOriginalName(header.Filename), nil
}

// receiptFile turns a stored path back into one the local filesystem understands: a
// no-op on Linux, and the inverse of the ToSlash above on Windows.
func receiptFile(stored string) string {
	return filepath.FromSlash(stored)
}

// handleImportRedirect keeps the old /import path working.
func (s *Server) handleImportRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/expense", http.StatusMovedPermanently)
}

// handleReceiptUpload stores the image, queues it and returns immediately.
func (s *Server) handleReceiptUpload(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	path, name, err := s.saveReceipt(r, user.ID)
	if err != nil {
		s.redirectError(w, r, "/expense", err.Error())
		return
	}
	if path == "" {
		s.redirectError(w, r, "/expense", "Choose a receipt image or PDF to upload.")
		return
	}

	if _, err := s.store.EnqueueReceipt(r.Context(), scopeOf(r), path, name); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Nudge the worker so it starts now rather than on its next tick.
	s.wakeWorker()

	s.flashSuccess(w, r,
		"Receipt uploaded. It is being processed in the background — carry on, and you will be told when it is ready.")
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleReceipt serves a stored receipt to the user who owns it.
func (s *Server) handleReceiptDiscard(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	path, err := s.store.DiscardReceipt(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.redirectError(w, r, "/expense", "That receipt has already been used or is no longer there.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The row is gone, so the file is unreachable either way and removing it is
	// housekeeping rather than correctness.
	if path != "" {
		if err := os.Remove(receiptFile(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("receipts: discarded job %d but could not delete %s: %v", id, path, err)
		}
	}

	s.redirectSuccess(w, r, "/expense", "Receipt discarded.")
}

// handleReceiptPreview serves the image of a receipt that has not yet become an
// expense, so the confirmation form can show the photograph beside the figures
// read off it. Checking a number against the picture is the whole point of
// confirming, and it cannot be done from memory.
//
// This is the one place in the application that serves a stored file inline
// rather than as an attachment, so it is worth being explicit about why that is
// safe here. handleReceipt below sends Content-Disposition: attachment because
// an HTML or SVG file rendered in this origin could run script with access to
// the session cookie. Here the bytes are sniffed first and only ever labelled
// with a raster image type this server recognised itself -- never a type taken
// from the upload -- and X-Content-Type-Options: nosniff stops the browser
// second-guessing that label. A JPEG cannot execute anything, and anything that
// is not one of the four raster formats is refused rather than served.
func (s *Server) handleReceiptPreview(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// Scoped to the household, so a guessed id returns 404 rather than somebody
	// else's shopping.
	job, err := s.store.UnattachedReceipt(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	f, err := os.Open(receiptFile(job.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	head := make([]byte, 32)
	n, _ := f.Read(head)
	kind := ocr.Sniff(head[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Only formats a browser will draw in an <img>, and only ones whose type
	// this server determined for itself.
	switch kind {
	case ocr.KindJPEG, ocr.KindPNG, ocr.KindGIF, ocr.KindWebP:
	default:
		// A PDF or a HEIC is a perfectly good receipt but not a previewable one,
		// and guessing would either fail silently or serve a type that is not
		// what the bytes are.
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", string(kind))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A receipt is one person's shopping. Private stops a shared proxy caching
	// it, and the URL is only reachable by a member of the household anyway.
	w.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(w, r, filepath.Base(job.Path), info.ModTime(), f)
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
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
		// The row survives even if the file is gone, so this is a 404 rather than a 500.
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Content-Disposition attachment plus nosniff means a stored file is downloaded rather
	// than rendered in this origin, where an SVG or HTML file would run script with access
	// to the site's cookies.
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+safeDownloadName(t.ReceiptName, t.ReceiptPath)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(t.ReceiptPath), info.ModTime(), f)
}

func randomFilename(ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b) + ext, nil
}

// receiptLabel turns an uploaded filename into a first guess at a description.
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

// Shared budgeting: the settings page and the actions on it.

// householdView backs /household.
type householdView struct {
	view

	Members []store.Member
	Pending []store.Invite

	// Roles are the roles an owner may assign, for the per-member selector.
	Roles []store.Role

	// Activity is the household's recent history: who changed what, and when.
	Activity []store.AuditEntry

	// MailEnabled drives what the page says about delivery.
	MailEnabled bool
}

// recentActivityLimit is how many history entries the sharing page shows.
const recentActivityLimit = 3

// assignableRoles excludes nothing: an owner may promote another member to
// owner, which is how ownership is handed over before somebody leaves.
func assignableRoles() []store.Role {
	return []store.Role{store.RoleOwner, store.RoleEditor, store.RoleViewer}
}

// handleHousehold shows who can see this budget, readable by every member including a
// viewer: somebody who can see the numbers should be able to see who else can.
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

	// Pending invitations are the owner's business: a viewer has no action to take on
	// them, and the addresses of people who have not yet accepted are not theirs to read.
	if hh.Role.CanManageMembers() {
		if v.Pending, err = s.store.PendingInvites(r.Context(), hh.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	// The history is for every member, not only the owner: who deleted the rent entry is
	// exactly what an editor needs answered.
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
		if len(v.Activity) == recentActivityLimit {
			break
		}
	}

	s.render(w, r, "household.html", v)
}

// handleHouseholdCreate makes a new shared budget and switches to it.
func (s *Server) handleHouseholdCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if !s.parseForm(w, r) {
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if _, err := s.store.CreateSharedHousehold(r.Context(), user.ID, name); err != nil {
		s.redirectError(w, r, "/household", err.Error())
		return
	}

	s.redirectSuccess(w, r, "/household", fmt.Sprintf("Created %q. Invite someone to join it.", name))
}

// handleHouseholdSwitch changes which budget the user is looking at.
func (s *Server) handleHouseholdSwitch(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if !s.parseForm(w, r) {
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

	if !s.parseForm(w, r) {
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
		s.redirectError(w, r, "/household", "This is your own budget, so it cannot be deleted.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, "/dashboard", fmt.Sprintf("Deleted %q. You are back in your own budget.", hh.Name))
}

// handleHouseholdLeave is a member removing themselves from a shared budget.
func (s *Server) handleHouseholdLeave(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	switch err := s.store.LeaveHousehold(r.Context(), hh.ID, user.ID); {
	case errors.Is(err, store.ErrPersonalHousehold):
		s.redirectError(w, r, "/household", "This is your own budget, so there is nothing to leave.")
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

	s.redirectSuccess(w, r, "/dashboard", fmt.Sprintf("You have left %q.", hh.Name))
}

// ── invitations ───────────────────────────────────────────────────────────────

// handleInviteCreate invites an email address to the current budget.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if !s.parseForm(w, r) {
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	role := store.Role(r.PostFormValue("role"))

	// The same address validation the login form uses, so "kushith" is rejected
	// here for the same reason and with the same wording it is rejected there.
	if msg := validateEmail(email); msg != "" {
		s.redirectError(w, r, "/household", msg)
		return
	}

	// Inviting a personal budget makes no sense: it is one person's private space.
	if hh.Personal {
		s.flashError(w, r,
			"This is your own private budget. Create a shared budget below, then invite people to that.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	if store.NormalizeEmail(email) == store.NormalizeEmail(user.Email) {
		s.redirectError(w, r, "/household", "You are already in this budget.")
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

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
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
func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	switch err := s.store.AcceptInvite(r.Context(), id, user.ID, user.Email); {
	case errors.Is(err, store.ErrInviteExpired):
		// Distinguished from "no longer available" on purpose.
		s.flashError(w, r,
			"That invitation has expired. Ask them to send it again — invitations last 24 hours.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrNotFound):
		// Same message whether the invitation never existed, was withdrawn, or
		// belongs to somebody else -- a probing user learns nothing either way.
		s.redirectError(w, r, "/dashboard", "That invitation is no longer available.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, "/dashboard", "You have joined. This is the shared budget.")
}

// handleInviteDecline refuses an invitation.
func (s *Server) handleInviteDecline(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	if err := s.store.DeclineInvite(r.Context(), id, user.Email); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, backTo(r), "Invitation declined.")
}

// ── members ───────────────────────────────────────────────────────────────────

// handleMemberRole changes one member's role.
func (s *Server) handleMemberRole(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if !s.parseForm(w, r) {
		return
	}

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
		return
	}

	role := store.Role(r.PostFormValue("role"))
	if !role.Valid() {
		s.redirectError(w, r, "/household", "Choose Owner, Editor or Viewer.")
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
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
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
		// The invitation row exists and is perfectly usable, so this is not a failure of the
		// invitation -- only of its delivery.
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

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	// The household id is passed in, so an owner of one budget cannot refresh an
	// invitation belonging to another.
	inv, err := s.store.ResendInvite(r.Context(), hh.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.redirectError(w, r, "/household", "That invitation is no longer waiting for an answer.")
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

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
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

// handleForgotRequest starts a password reset. The response is identical whether or not
// the address has an account -- a different message, status code or response time would
// make this an oracle for which addresses are registered.
func (s *Server) handleForgotRequest(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
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

	// Rate limited on IP alone, not on the address.
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

	// Detached from the request context on purpose: this outlives the response, which is
	// what keeps the timing uniform.
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

	// Invalid means the link is expired, already used, or was never real.
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
	if !s.parseForm(w, r) {
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

	// ConsumeReset burns the token, sets the hash and deletes every session for the
	// account in one transaction.
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
	s.redirectSuccess(w, r, "/", "Password changed. Please sign in with your new password.")
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

	if !s.parseForm(w, r) {
		return
	}
	current := r.PostFormValue("current")
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	// The current password is required even though the session already proves who this is.
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

	// This session is kept and the others dropped: signing someone out of the page they are
	// looking at, moments after they proved they know the password, would be gratuitous.
	if err := s.store.ChangePassword(r.Context(), user.ID, string(newHash), currentSession(r)); err != nil {
		s.serverError(w, r, err)
		return
	}

	log.Printf("auth: user %d changed their password; other sessions revoked", user.ID)
	s.redirectSuccess(w, r, "/sessions", "Password changed. Any other device has been signed out.")
}

// conflictOnUpdate re-renders the edit form after a losing race, showing what the other
// person saved and carrying the new version forward so that saving again succeeds.
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
