package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

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
