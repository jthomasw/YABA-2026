package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// pages lists every top-level template. Each one defines a "content" block
// that layout.html renders inside the shared chrome.
var pages = []string{
	"landing.html",  // sign-in page
	"register.html", // account creation page
	"setup.html",    // first-account budget setup
	"about.html",    // the `Learn more!` link
	"forgot.html",   // the `Forgot password` link
	"dashboard.html",
	"reports.html",
	"transactions.html",
	"transaction_form.html",
	"income.html",    // dedicated Add Income page
	"expense.html",   // dedicated Add Expense page, opening on Manual or Upload
	"receipts.html",  // receipts uploaded but not yet entered
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
	// overBy turns a negative "remaining" into the positive amount it is over
	// by: "$20.00 over", not "-$20.00 over".
	"overBy": func(c money.Cents) money.Cents {
		if c < 0 {
			return -c
		}
		return c
	},
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
	// frequencyLabel turns a (n, unit) recurrence into "Every 2 weeks", the same
	// wording the presets on the income and expense forms already use, so a
	// saved schedule reads back the way it was chosen rather than as raw numbers.
	"frequencyLabel": func(n int, unit string) string {
		if n == 1 {
			return "Every " + unit
		}
		return fmt.Sprintf("Every %d %ss", n, unit)
	},
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
	// HouseholdID identifies it: names are not unique (two people can each
	// invite you to a budget called "Home"), so anything that has to pick the
	// current budget out of a list compares this, never the name.
	Household   string
	HouseholdID int64
	Role        store.Role
	Personal    bool

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

	// no-store on every rendered page. Forms embed a CSRF token tied to the session and
	// signing in rotates it, so a cached page fails the check with a baffling message.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	// Every script is same-origin: Chart.js is vendored into /static rather than
	// pulled from a CDN, so no third party can reach the page. The other
	// security headers are set for every response in securityHeaders.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			// No 'unsafe-inline': every script is a file under /static and
			// no template uses an on*= attribute, so an injected <script> or
			// handler will not run. Inline styles are still allowed; they
			// cannot execute code.
			"script-src 'self'; "+
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
			v.Household, v.HouseholdID, v.Role, v.Personal = m.Name, m.ID, m.Role, m.Personal
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
			proof := store.TokenHash(s.inviteProof(r))
			for i := range inv {
				inv[i].NeedsLink = inv[i].TokenHash != "" && inv[i].TokenHash != proof &&
					!inv[i].AccountPredates
			}
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
	id := requestID(r.Context())
	log.Printf("ERROR %s %s %s: %v", id, r.Method, r.URL.Path, err)

	// The id is the only thing this page says that the log does not already
	// know -- and it is the thing that makes a user report actionable. Still
	// no detail about what failed.
	http.Error(w, "Something went wrong on our end. Please try again.\n\n"+
		"If it keeps happening, quote reference "+id+" so it can be looked up.",
		http.StatusInternalServerError)
}

// safeMessage decides what a store error may say to the user, and makes sure
// the real one is written down either way.
//
// Fourteen handlers used to put err.Error() straight into a flash. Most of the
// time that is right -- "a fund needs a name", "not enough available cash" --
// and replacing it with something generic would make the app worse. But the
// same store functions wrap driver failures, so the same line could render
// `set budget: constraint failed: UNIQUE constraint failed:
// budgets.household_id, budgets.category (2067)` on the page: the storage
// engine, the table and the columns, handed to anybody who can trip it. And
// because flashError does no logging, the operator saw nothing at all while the
// user read the SQL.
//
// The discriminator is the error chain, not a list of strings to keep in step.
// The store writes a validation failure as a leaf -- fmt.Errorf with no %w -- or
// as one of its own sentinels, while anything from the driver arrives wrapped:
// fmt.Errorf("create fund: %w", err). So a leaf, or something built on a
// sentinel, is the store talking to the user; anything else is the database
// talking to us, and the user gets the fallback instead.
//
// Whatever is shown, the full error is logged with the route that produced it.
func (s *Server) safeMessage(r *http.Request, err error, fallback string) string {
	if err == nil {
		return ""
	}

	if isUserFacing(err) {
		return err.Error()
	}

	log.Printf("ERROR %s %s %s: %v", requestID(r.Context()), r.Method, r.URL.Path, err)
	return fallback
}

// isUserFacing reports whether an error was written to be read by the person who
// caused it. See safeMessage.
func isUserFacing(err error) bool {
	// Known sentinels are safe however they are wrapped: they exist precisely so
	// a handler can recognise them, and their text is written for the user.
	for _, sentinel := range []error{
		store.ErrNotFound,
		store.ErrConflict,
		store.ErrEmailTaken,
		store.ErrInsufficientCash,
		store.ErrInsufficientFund,
		store.ErrItemsDoNotBalance,
		store.ErrNotMember,
		store.ErrForbidden,
		store.ErrLastOwner,
		store.ErrAlreadyMember,
		store.ErrInviteOpen,
		store.ErrInviteExpired,
		store.ErrPersonalHousehold,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	// Anything from the database itself is never the user's to read, however
	// it arrives. The SQLite driver's errors wrap nothing, so the leaf rule
	// below used to let "database is locked (5)" or "constraint failed: ...
	// (1811)" straight through when a store path returned one unwrapped.
	if isInternal(err) {
		return false
	}

	// Otherwise: a leaf is the store's own validation message; anything wrapping
	// something else came from underneath and is not ours to show.
	return errors.Unwrap(err) == nil
}

// isInternal reports whether err comes from the database, the filesystem or
// the request's own plumbing rather than from the app's own checks.
func isInternal(err error) bool {
	var sqliteErr *sqlite.Error
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &sqliteErr), errors.As(err, &pathErr):
		return true
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, sql.ErrTxDone), errors.Is(err, sql.ErrConnDone),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return true
	}
	return false
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
