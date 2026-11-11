package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

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

	// sessionInviteProof holds the secret from an invitation's email link once
	// it has been opened in this browser; it is what lets Accept work. It
	// survives signing in, so the link can be opened before logging in.
	sessionInviteProof = "invite_proof"
)

// userCtxKey is unexported so no other package can plant a user in the context.
type userCtxKey struct{}

// requestIDCtxKey carries the per-request id logRequests assigns, so an error
// logged deep in a handler can name the same request the access log does.
type requestIDCtxKey struct{}

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

		// The body is already bounded by limitBodies.
		if !isSafeMethod(r.Method) {
			// Read the body now, so an oversized one is reported as what it
			// is. Left to checkCSRF, the parse failed quietly, the token was
			// never found, and somebody who had picked a large photo was told
			// "This form has expired" -- and went back to retype everything
			// only to hit the same wall. Parsing here changes nothing else:
			// checkCSRF parsed the body at this point anyway, with the same
			// limit, and the handler reads the already-parsed form.
			if bodyTooLarge(s.parseBody(r)) {
				log.Printf("request refused: %s %s user=%d: body over %d bytes",
					r.Method, r.URL.Path, uid, s.bodyLimit(r.URL.Path))
				http.Error(w, s.tooLargeMessage(r.URL.Path), http.StatusRequestEntityTooLarge)
				return
			}
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

		// A short id per request, put in the context and echoed in a header.
		//
		// Without one, "it failed at about three o'clock" could not be tied to
		// any of the ERROR lines: the access log had method, path, status and
		// duration, and the error log had a message, and nothing joined them.
		// Now both carry the same token and the user can read it off the page
		// they were sent.
		id := newRequestID()
		r = r.WithContext(context.WithValue(r.Context(), requestIDCtxKey{}, id))
		w.Header().Set("X-Request-Id", id)

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// Query strings are omitted: they can carry a search term, and access
		// logs are the classic place sensitive input leaks into plain text.
		log.Printf("INFO  %s %s %s %d %s", id, r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond))
	})
}

// newRequestID returns 8 hex characters: short enough for somebody to read out
// over the phone, wide enough that two requests in the same log file will not
// collide in practice. It is not a secret and carries nothing about the user.
func newRequestID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failure here must not fail the request. A timestamp is a worse id
		// than random bytes and a far better one than none.
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(b[:])
}

// requestID returns the id logRequests assigned, or "-" outside a request.
func requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDCtxKey{}).(string); ok && id != "" {
		return id
	}
	return "-"
}

// handleHealth is the liveness and readiness probe.
//
// There was nothing to probe before but GET /, which renders a template and
// touches the session store -- so a load balancer could not tell a healthy
// process from one whose database had seized, and neither could an operator.
// This does the one check that distinguishes them: a trivial query, with a
// timeout, on the single pooled connection every other request needs.
//
// Deliberately public and deliberately uninformative: 200 "ok" or 503, no
// version, no schema number, no error text. A probe does not need them and an
// unauthenticated visitor should not have them.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if err := s.store.Ping(ctx); err != nil {
		log.Printf("ERROR %s health: database unreachable: %v", requestID(r.Context()), err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable\n"))
		return
	}

	_, _ = w.Write([]byte("ok\n"))
}
