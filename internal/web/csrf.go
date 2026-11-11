package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/sessions"
)

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
	// A path beginning "//" is not a path to a browser, it is a
	// protocol-relative URL: Location: //evil.example/x navigates off this site
	// entirely, while still passing a naive "starts with /" check. Same for a
	// "/\" , which several browsers normalise to "//".
	if strings.HasPrefix(u.Path, "//") || strings.HasPrefix(u.Path, "/\\") {
		return "/dashboard"
	}
	// Drop scheme, host and userinfo: whatever is left cannot leave this site.
	back := u.Path
	if u.RawQuery != "" {
		back += "?" + u.RawQuery
	}
	return back
}

// sameSiteNavigation reports whether a request came from this site rather than
// from somebody else's page.
//
// Two pages -- /income and /expense -- catch recurring schedules up on the way
// in, which makes them GETs that write, in an app whose route table otherwise
// promises the opposite. The writes are idempotent and only materialise what a
// schedule already owed, so the exposure was never invention of data; but an
// <img src="https://yaba.example/income"> on any page an editor visited still
// drove a write in their name, and the honest fix is to not do the write.
//
// Sec-Fetch-Site is sent by every current browser and cannot be set or removed
// by the page making the request, which is exactly the property needed here. A
// request without it is something else -- curl, an old browser, a health check
// -- and is treated as same-site, because refusing those would break the page
// for a real user in order to close a hole only a browser can be walked into.
func sameSiteNavigation(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		// "same-site" is a different origin on the same registrable domain --
		// another subdomain. Close enough to elsewhere to decline the write.
		return false
	default:
		// "same-origin", "none" (the user typed it or used a bookmark), or absent.
		return true
	}
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

// parseBody parses a POST body, multipart or URL-encoded, with the same
// memory limit checkCSRF uses, and returns the parse error.
func (s *Server) parseBody(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(s.maxUploadBytes())
	}
	return r.ParseForm()
}

// bodyTooLarge reports whether a body parse failed because the request was
// bigger than http.MaxBytesReader allows. The multipart reader does not wrap
// every error it passes on, so the message is matched as a fallback.
func bodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large")
}

// tooLargeMessage is what an oversized submission is told, naming the limit
// actually in force rather than the configured value, which may be unset.
func (s *Server) tooLargeMessage(path string) string {
	if !uploadRoutes[path] {
		return "That form is too large to accept. Go back, shorten what you entered, and try again."
	}
	return fmt.Sprintf("That upload is too large. Receipts can be at most %d MB. "+
		"Go back, choose a smaller file -- a lower-resolution photo usually works -- and try again.",
		s.maxUploadMB())
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

// publicCSRFOK is the CSRF check for the POST routes that cannot sit behind
// authed, because the people who need them are precisely the ones who cannot
// sign in: /forgot and /reset.
//
// Those two rendered a token into their forms and then never looked at it. The
// consequence on /forgot was small but real -- any page anywhere could POST an
// address and spend that visitor's per-IP reset budget, so their own genuine
// request would be refused -- and on /reset it was the principle rather than an
// attack: forging that POST needs a valid token, which an attacker only holds
// for their own account.
//
// A missing or mismatched token is a false return; the caller decides what to
// render, because these two pages report errors differently.
func (s *Server) publicCSRFOK(r *http.Request) bool {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		// An undecodable cookie is not a pass. The visitor gets the "that form
		// expired" path, which is the truth: their session is unreadable.
		return false
	}
	return s.checkCSRF(r, session)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
