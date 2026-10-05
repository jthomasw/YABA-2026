package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/store"
)

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

	// The form has always carried a token; until now nothing read it, so any
	// third-party page could POST an address here and burn this visitor's
	// per-IP reset budget.
	if !s.publicCSRFOK(r) {
		v.Error = "That form expired. Please try again."
		s.renderStatus(w, r, http.StatusForbidden, "forgot.html", v)
		return
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
	if errors.Is(err, store.ErrNotFound) {
		v.Invalid = true
		s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
		return
	}
	if err != nil {
		// A database failure is not a forged link. Telling somebody with a
		// perfectly good link that it "was never real" sends them round the
		// loop to request another and be told the same thing, while nothing
		// was written down to explain why.
		s.serverError(w, r, err)
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

	if !s.publicCSRFOK(r) {
		v.Error = "That form expired. Please open the link from your email again."
		s.renderStatus(w, r, http.StatusForbidden, "reset.html", v)
		return
	}

	// Resolve first, so an expired link is reported as expired rather than as a
	// password problem.
	user, err := s.store.ResetUser(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		v.Invalid = true
		s.renderStatus(w, r, http.StatusBadRequest, "reset.html", v)
		return
	}
	if err != nil {
		// A database failure is not a forged link. Telling somebody with a
		// perfectly good link that it "was never real" sends them round the
		// loop to request another and be told the same thing, while nothing
		// was written down to explain why.
		s.serverError(w, r, err)
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

	// Rate limited like a login, because that is exactly what this is: an
	// online guess against a bcrypt hash. The lockout is checked BEFORE the
	// password, keyed on the account alone. Checking it only after a wrong
	// guess meant a locked-out caller could keep guessing, and the right
	// password still went through -- so the limit stopped nothing.
	key := "pwchange|" + strconv.FormatInt(user.ID, 10)
	retryIn, err := s.store.RateRetryIn(r.Context(), key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if retryIn > 0 {
		s.renderPasswordForm(w, r, http.StatusTooManyRequests,
			"Too many failed attempts. Try again "+retryPhrase(retryIn)+".")
		return
	}

	// The current password is required even though the session already proves who this is.
	_, hash, err := s.store.CredentialsFor(r.Context(), user.Email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		if err := s.store.RateFail(r.Context(), key); err != nil {
			log.Printf("WARN  %s could not count a failed password check: %v", requestID(r.Context()), err)
		}
		s.renderPasswordForm(w, r, http.StatusUnauthorized,
			"That is not your current password.")
		return
	}
	if err := s.store.RateReset(r.Context(), key); err != nil {
		log.Printf("WARN  %s could not clear the password-check counter: %v", requestID(r.Context()), err)
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
