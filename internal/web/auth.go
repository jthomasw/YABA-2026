package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// landingView backs the sign-in page.
type landingView struct {
	view

	Error string
	Email string
}

type registerView struct {
	view
	Error string
	Email string
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.signedIn(r) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	v := landingView{view: s.baseView(w, r, "Welcome to YABA", "landing")}
	s.render(w, r, "landing.html", v)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.signedIn(r) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.renderRegister(w, r, http.StatusOK, registerView{})
}

// handleAuth handles sign-in only. Account creation has its own page and endpoint.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}

	// The landing form is unauthenticated but still carries a token, minted when the page
	// was rendered.
	// An unreadable cookie fails the check too: skipping it in that case let a
	// forged sign-in through whenever the visitor's cookie could not be decoded.
	if !s.publicCSRFOK(r) {
		s.renderLanding(w, r, http.StatusForbidden, landingView{
			Email: r.PostFormValue("email"),
			Error: "That form expired. Please try again.",
		})
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")

	if email == "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Error: "Please enter your email address."})
		return
	}

	// The address is checked for shape before it is looked up, so a value that is not an
	// email is rejected whether or not an account matches it.
	if msg := validateEmail(email); msg != "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email, Error: msg})
		return
	}

	if password == "" {
		s.renderLanding(w, r, http.StatusBadRequest, landingView{
			Email: email,
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

	s.attemptLogin(w, r, email, password, limit)
}

func (s *Server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	if !s.publicCSRFOK(r) {
		s.renderRegister(w, r, http.StatusForbidden, registerView{
			Email: r.PostFormValue("email"), Error: "That form expired. Please try again.",
		})
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")
	showError := func(status int, message string) {
		s.renderRegister(w, r, status, registerView{Email: email, Error: message})
	}

	// Registration is rate limited on the same counters as sign-in.
	//
	// Two reasons, and the first is the one that matters. This page answers
	// "does this address have an account here?" out loud -- 409 and a sentence
	// saying so -- which is the exact question /auth and /forgot go to real
	// trouble to refuse. Registration cannot honestly refuse it as well without
	// an email round-trip this deployment cannot rely on, so the fact stays
	// available and the RATE does not: an attacker can confirm one address they
	// already suspect, but cannot walk a list of ten thousand.
	//
	// The second: every new address costs a bcrypt at DefaultCost. Unlimited,
	// that is a CPU exhaustion lever anybody can pull from a browser tab.
	limit := newLoginLimit(r, email)
	retryIn, err := s.retryIn(r.Context(), limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if retryIn > 0 {
		showError(http.StatusTooManyRequests,
			"Too many attempts from this device. Try again "+retryPhrase(retryIn)+".")
		return
	}

	if msg := validateEmail(email); msg != "" {
		showError(http.StatusBadRequest, msg)
		return
	}
	if msg := validatePassword(password); msg != "" {
		showError(http.StatusBadRequest, msg)
		return
	}
	if password != confirm {
		showError(http.StatusBadRequest, "The two passwords do not match.")
		return
	}
	exists, err := s.store.EmailExists(r.Context(), email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if exists {
		// This answer is the one worth paying for, so it is charged to both
		// counters -- probing costs the prober their budget.
		s.rateFail(r.Context(), limit)
		showError(http.StatusConflict, "An account with this email already exists. Please sign in.")
		return
	}

	// The account-creation slot is taken only now, once the form is valid and
	// an account is about to be made: store.SignupLimit counts accounts, not
	// attempts. Taking it first spent a slot on the "confirm password" step and
	// on every typo, so only two real sign-ups fitted in the hour. RateTake
	// checks and counts in one statement, so a burst of simultaneous requests
	// cannot all slip through on the same remaining slot.
	allowed, err := s.store.RateTake(r.Context(), signupKey(r), store.SignupLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !allowed {
		wait, err := s.store.RateRetryInFor(r.Context(), signupKey(r), store.SignupLimit)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		showError(http.StatusTooManyRequests,
			"Too many accounts have been created from this network. Try again "+retryPhrase(wait)+".")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	userID, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, store.ErrEmailTaken) {
		// Lost the race with a concurrent signup for the same address; same
		// answer, so the same charge.
		s.rateFail(r.Context(), limit)
		showError(http.StatusConflict, "An account with this email already exists. Please sign in.")
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

// signupKey is the counter for accounts created from one network address.
func signupKey(r *http.Request) string { return "signup|" + clientIP(r) }

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

// errSignInFailed is the only thing a failed sign-in ever says. Both branches
// below use it -- the address with no account, and the wrong password -- because
// a message that tells them apart turns the sign-in page into a tool for
// discovering who has an account here. It is the same reason the unknown-address
// branch still spends a bcrypt comparison it does not need: having gone to the
// trouble of equalising the timing, it would be a waste to give the answer away
// in words.
//
// A constant rather than the same sentence typed twice, so the two branches
// cannot drift apart again.
const errSignInFailed = "That email and password do not match."

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
			Email: email, Error: errSignInFailed})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.rateFail(r.Context(), limit)
		s.renderLanding(w, r, http.StatusUnauthorized, landingView{
			Email: email, Error: errSignInFailed})
		return
	}

	// Worth noticing when it fails: the counter then stays where it was, so the
	// user's next mistyped password can trip the lockout on the first try and
	// they are told "too many failed attempts" after one.
	if err := s.store.RateReset(r.Context(), limit.account); err != nil {
		log.Printf("WARN  %s could not clear the login counter after a successful sign-in: %v",
			requestID(r.Context()), err)
	}
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
	if err != nil {
		// Nothing readable to sign out of: just make sure the cookie is gone.
		s.clearSession(w, r)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.checkCSRF(r, session) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	// Delete the row, not just the cookie. Clearing the cookie leaves a live session that
	// anyone holding a copy of the token could keep using: signing out has to destroy the
	// credential, not merely forget it locally.
	if sid, ok := session.Values[sessionID].(string); ok && sid != "" {
		if u, uerr := s.store.SessionUser(r.Context(), sid); uerr == nil {
			if derr := s.store.DeleteSession(r.Context(), u.ID, sid); derr != nil &&
				!errors.Is(derr, store.ErrNotFound) {
				log.Printf("logout: could not delete session: %v", derr)
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
	if target == store.TokenHash(currentSession(r)) {
		s.redirectError(w, r, "/sessions", "That is this device. Use Log out instead.")
		return
	}

	// The form carries the session's hashed ID, never its token. The user id is
	// in the WHERE clause, so a guessed ID cannot sign somebody else out.
	switch err := s.store.DeleteSessionByID(r.Context(), user.ID, target); {
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

// duplicateSubmit reports whether this submission has already been processed,
// answering it itself (a redirect, or an error page) when it has -- the caller
// just returns on true.
//
// A database error is not "not a duplicate". It used to be: the save went
// ahead unprotected, so a double click during a slow moment could record the
// same money twice. Now the save is refused and the user retries.
func (s *Server) duplicateSubmit(w http.ResponseWriter, r *http.Request, userID int64, to, msg string) bool {
	tok := strings.TrimSpace(r.PostFormValue("form_token"))
	if tok == "" {
		return false
	}
	used, err := s.store.ConsumeFormToken(r.Context(), userID, tok)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("consume form token: %w", err))
		return true
	}
	if !used {
		s.redirectSuccess(w, r, to, msg)
		return true
	}
	return false
}

// restoreSubmissionToken lets a user correct or retry a save that failed before
// it created any data. It never runs after a successful transaction insert.
func (s *Server) restoreSubmissionToken(r *http.Request, userID int64) {
	if err := s.store.RestoreFormToken(r.Context(), userID, strings.TrimSpace(r.PostFormValue("form_token"))); err != nil {
		log.Printf("form token: could not restore token: %v", err)
	}
}

func formBelongsToHousehold(r *http.Request, householdID int64) bool {
	raw := strings.TrimSpace(r.PostFormValue("form_household"))
	if raw == "" {
		return true
	} // permits a short transition for existing open forms
	id, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && id == householdID
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

func (s *Server) renderRegister(w http.ResponseWriter, r *http.Request, status int, v registerView) {
	v.view = s.baseView(w, r, "Create an account", "register")
	s.renderStatus(w, r, status, "register.html", v)
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
