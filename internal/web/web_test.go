package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/mail"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// testRig is a live server, a real database and a cookie jar.
type testRig struct {
	t       *testing.T
	handler http.Handler
	store   *store.Store
	cookies []*http.Cookie
	userID  int64

	// db is the raw handle, used only to age a session row so the expiry path
	// can be tested without waiting thirty days.
	db *sql.DB

	// scope is the household the test user works in, resolved once so tests can
	// call the store directly with the same scope the handlers use.
	scope store.Scope

	// uploadDir is where the server writes receipts, so a test can put a real
	// file there and check that discarding it removes the file as well as the row.
	uploadDir string
}

const (
	testPassword = "correct-horse-battery"
	testEmail    = "tester@example.com"
)

// newRig builds a server with no mail relay, which is the common case.
func newRig(t *testing.T) *testRig {
	t.Helper()
	return newRigWithMail(t, false)
}

// newRigWithMail builds a server whose mailer either can or cannot send.
//
// A real *mail.Mailer either way rather than a stub: Enabled() is decided by
// whether a host and sender are configured, so constructing one with a host is
// the honest way to exercise the "mail works" branch. Nothing is dialled, since
// no test here asks it to send.
func newRigWithMail(t *testing.T, mailEnabled bool) *testRig {
	t.Helper()

	mailCfg := mail.Config{}
	if mailEnabled {
		mailCfg = mail.Config{
			Host: "smtp.example.test", From: "YABA <yaba@example.test>",
			BaseURL: "http://localhost:8000",
		}
	}
	uploadDir := t.TempDir()

	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st := store.New(sqlDB)
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid, err := st.CreateUser(context.Background(), testEmail, string(hash))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	srv, err := New(st, Config{
		Addr:        ":0",
		SessionKey:  []byte("test-key-that-is-at-least-32-bytes-long"),
		UploadDir:   uploadDir,
		MaxUploadMB: 1,
		Mail:        mail.New(mailCfg),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	m, err := st.ActiveHousehold(context.Background(), store.User{ID: uid})
	if err != nil {
		t.Fatalf("active household: %v", err)
	}

	return &testRig{
		t: t, handler: srv.Handler(), store: st, userID: uid, db: sqlDB,
		scope:     store.Scope{HouseholdID: m.ID, UserID: uid},
		uploadDir: uploadDir,
	}
}

// do issues a request carrying the jar's cookies and stores any it receives.
func (r *testRig) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	r.t.Helper()

	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)

	if got := rec.Result().Cookies(); len(got) > 0 {
		r.cookies = got
	}
	return rec
}

var csrfRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// csrf fetches a page and extracts its CSRF token.
func (r *testRig) csrf(target string) string {
	r.t.Helper()
	body := r.do("GET", target, nil).Body.String()
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		r.t.Fatalf("no CSRF token in %s", target)
	}
	return m[1]
}

// login authenticates the rig's user.
func (r *testRig) login() {
	r.t.Helper()
	r.loginAs(testEmail)
}

// loginAs authenticates a specific address, clearing any existing session first
// so a test can switch identity without the previous cookie leaking through.
func (r *testRig) loginAs(email string) {
	r.t.Helper()
	r.cookies = nil
	token := r.csrf("/")

	rec := r.do("POST", "/auth", url.Values{
		"csrf_token": {token},
		"email":      {email},
		"password":   {testPassword},
	})
	if rec.Code != http.StatusSeeOther {
		r.t.Fatalf("login %s: status %d, body %q", email, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		r.t.Fatalf("login %s redirected to %q", email, got)
	}
}

// expireSession ages a row past both the absolute and the idle limit, so the
// refusal path is testable without waiting thirty days.
func (r *testRig) expireSession(id string) {
	r.t.Helper()
	if _, err := r.db.Exec(`
		UPDATE sessions
		SET expires_at   = datetime('now', '-1 day'),
		    last_seen_at = datetime('now', '-40 days')
		WHERE id = ?`, id); err != nil {
		r.t.Fatalf("expire session: %v", err)
	}
}

// post issues a form POST with a freshly fetched CSRF token, so a permission
// test cannot pass merely because its token was stale.
func (r *testRig) post(target string, form url.Values) *httptest.ResponseRecorder {
	r.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", r.csrf("/dashboard"))
	return r.do("POST", target, form)
}

// ── revocable sessions ────────────────────────────────────────────────────────

// TestSessionRevocationIsImmediate is the whole point of the sessions table.
//
// Before it existed a login was a signed cookie with no server-side record, so
// it could be verified but never cancelled. This asserts the property that was
// missing: deleting the row ends that login on its very next request.
func TestSessionRevocationIsImmediate(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// The session works.
	if rec := rig.do("GET", "/dashboard", nil); rec.Code != http.StatusOK {
		t.Fatalf("dashboard before revocation: %d, want 200", rec.Code)
	}

	sessions, err := rig.store.Sessions(context.Background(), rig.userID, "")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected exactly 1 session after login, got %d", len(sessions))
	}

	// Revoke it out of band, as a device list or a password change would.
	if err := rig.store.DeleteSession(context.Background(), rig.userID, sessions[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The very next request must be turned away, with the same cookie.
	rec := rig.do("GET", "/dashboard", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("dashboard after revocation: %d, want a redirect to login", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("redirected to %q, want %q", got, "/")
	}
}

// TestPasswordChangeEndsEverySession covers the case that made the old design
// indefensible: changing a password locked nobody out.
func TestPasswordChangeEndsEverySession(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// A second login for the same account, standing in for another device.
	second, err := rig.store.CreateSession(context.Background(), rig.userID, "Other device")
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}

	n, err := rig.store.DeleteUserSessions(context.Background(), rig.userID)
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d sessions, want 2", n)
	}

	// Neither the browser session nor the other device resolves any more.
	if rec := rig.do("GET", "/dashboard", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("this device still authenticated: %d", rec.Code)
	}
	if _, err := rig.store.SessionUser(context.Background(), second); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other device still resolves: %v", err)
	}
}

// TestRevokeOthersKeepsThisDevice covers the "sign out everywhere else" case.
func TestRevokeOthersKeepsThisDevice(t *testing.T) {
	rig := newRig(t)
	rig.login()

	mine, err := rig.store.Sessions(context.Background(), rig.userID, "")
	if err != nil || len(mine) != 1 {
		t.Fatalf("expected 1 session, got %d (%v)", len(mine), err)
	}
	keep := mine[0].ID

	for i := 0; i < 3; i++ {
		if _, err := rig.store.CreateSession(context.Background(), rig.userID, "Device"); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	n, err := rig.store.DeleteOtherSessions(context.Background(), rig.userID, keep)
	if err != nil {
		t.Fatalf("revoke others: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked %d, want 3", n)
	}

	// Still logged in here.
	if rec := rig.do("GET", "/dashboard", nil); rec.Code != http.StatusOK {
		t.Errorf("this device was signed out too: %d", rec.Code)
	}
	if _, err := rig.store.SessionUser(context.Background(), keep); err != nil {
		t.Errorf("kept session no longer resolves: %v", err)
	}
}

// TestOneUserCannotRevokeAnothersSession: the device list acts on your own
// logins only, so the user id is part of every WHERE clause.
func TestOneUserCannotRevokeAnothersSession(t *testing.T) {
	rig := newRig(t)
	rig.login()

	victim, err := rig.store.CreateUser(context.Background(), "victim@example.com", "hash")
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	theirs, err := rig.store.CreateSession(context.Background(), victim, "Victim device")
	if err != nil {
		t.Fatalf("create victim session: %v", err)
	}

	// Attacker knows the token and posts it. The handler scopes the delete to the
	// caller, so nothing happens.
	rig.post("/sessions/revoke", url.Values{"session_id": {theirs}})

	if _, err := rig.store.SessionUser(context.Background(), theirs); err != nil {
		t.Errorf("another user's session was revoked: %v", err)
	}
}

// TestLogoutDeletesTheRow: clearing the cookie is not enough, because anyone
// holding a copy of the token could keep using it.
func TestLogoutDeletesTheRow(t *testing.T) {
	rig := newRig(t)
	rig.login()

	before, err := rig.store.Sessions(context.Background(), rig.userID, "")
	if err != nil || len(before) != 1 {
		t.Fatalf("expected 1 session, got %d (%v)", len(before), err)
	}
	token := before[0].ID

	rig.post("/logout", nil)

	if _, err := rig.store.SessionUser(context.Background(), token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session survived logout: %v", err)
	}
}

// TestExpiredSessionIsRefused checks the time conditions in SessionUser, by
// ageing a row past both limits directly.
func TestExpiredSessionIsRefused(t *testing.T) {
	rig := newRig(t)

	id, err := rig.store.CreateSession(context.Background(), rig.userID, "Old browser")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rig.store.SessionUser(context.Background(), id); err != nil {
		t.Fatalf("fresh session should resolve: %v", err)
	}

	rig.expireSession(id)

	if _, err := rig.store.SessionUser(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session still resolves: %v", err)
	}

	// And the purge collects it.
	n, err := rig.store.PurgeExpiredSessions(context.Background())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n == 0 {
		t.Error("purge removed nothing, expected the expired row")
	}
}

// TestUnknownSessionTokenIsRefused: a random token must not authenticate, and
// must be indistinguishable from a revoked one.
func TestUnknownSessionTokenIsRefused(t *testing.T) {
	rig := newRig(t)
	for _, tok := range []string{"", "not-a-real-token", "0000000000000000000000000000000000000000000"} {
		if _, err := rig.store.SessionUser(context.Background(), tok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("token %q gave %v, want ErrNotFound", tok, err)
		}
	}
}

// TestDeviceListShowsThisDevice: the page must mark which row you are on, or
// signing out the wrong one is easy.
func TestDeviceListShowsThisDevice(t *testing.T) {
	rig := newRig(t)
	rig.login()
	if _, err := rig.store.CreateSession(context.Background(), rig.userID, "Firefox on Linux"); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := rig.do("GET", "/sessions", nil).Body.String()
	if !strings.Contains(body, "This device") {
		t.Error("device list does not mark the current session")
	}
	if !strings.Contains(body, "Firefox on Linux") {
		t.Error("device list does not show the other session's user agent")
	}
	if !strings.Contains(body, "Sign out everywhere else") {
		t.Error("device list should offer to revoke the others")
	}
}

// ── shared budgeting: the permission matrix ───────────────────────────────────

// addMember creates an account, drops it into a household with a role, and
// returns its email.
//
// The membership is written through the store rather than through the invitation
// flow, because the subject of these tests is enforcement, not onboarding: a bug
// in the invite pages should not be able to make a permission test pass.
func (r *testRig) addMember(hh int64, email string, role store.Role) string {
	r.t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		r.t.Fatalf("hash: %v", err)
	}
	uid, err := r.store.CreateUser(context.Background(), email, string(hash))
	if err != nil {
		r.t.Fatalf("create %s: %v", email, err)
	}
	if err := r.store.InviteMember(context.Background(), hh, r.userID, email, role); err != nil {
		r.t.Fatalf("invite %s: %v", email, err)
	}
	invites, err := r.store.InvitesFor(context.Background(), uid, email)
	if err != nil || len(invites) != 1 {
		r.t.Fatalf("invites for %s: %v (%d found)", email, err, len(invites))
	}
	if err := r.store.AcceptInvite(context.Background(), invites[0].ID, uid, email); err != nil {
		r.t.Fatalf("accept for %s: %v", email, err)
	}
	return email
}

// sharedRig returns a rig whose test user owns a shared household, plus an
// editor and a viewer in it.
func sharedRig(t *testing.T) (*testRig, int64, string, string) {
	t.Helper()
	rig := newRig(t)
	rig.login()

	hh, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Flat 4B")
	if err != nil {
		t.Fatalf("create shared household: %v", err)
	}
	editor := rig.addMember(hh, "editor@example.com", store.RoleEditor)
	viewer := rig.addMember(hh, "viewer@example.com", store.RoleViewer)
	return rig, hh, editor, viewer
}

// TestPermissionMatrix is the test this whole feature rests on.
//
// Shared budgeting is the first change in this project that makes it possible for
// one person to be shown another's money, so the rules are asserted end to end
// through real HTTP requests rather than by unit-testing Role's methods -- what
// matters is that the ROUTES enforce them, and a route registered with the wrong
// wrapper is exactly the mistake a Role unit test would sail past.
//
// A refused POST redirects with an explanation rather than 403ing, so the
// assertion is "did the write happen", not "what was the status code".
func TestPermissionMatrix(t *testing.T) {
	rig, _, editor, viewer := sharedRig(t)

	cases := []struct {
		role   store.Role
		email  string
		target string
		form   url.Values
		allow  bool
		what   string
	}{
		// An editor records money and plans, but does not move savings.
		{store.RoleEditor, editor, "/expense",
			url.Values{"label": {"Food"}, "amount": {"12.00"}, "date": {"2026-03-01"}},
			true, "editor adds an expense"},
		{store.RoleEditor, editor, "/income",
			url.Values{"label": {"Salary"}, "amount": {"900.00"}, "date": {"2026-03-01"}},
			true, "editor adds income"},
		{store.RoleEditor, editor, "/funds",
			url.Values{"name": {"Holiday"}, "goal": {"500"}},
			true, "editor creates a fund"},
		{store.RoleEditor, editor, "/budgets",
			url.Values{"category": {"Food"}, "limit": {"200"}},
			true, "editor sets a category budget"},

		// A viewer changes nothing at all.
		{store.RoleViewer, viewer, "/expense",
			url.Values{"label": {"Sneaky"}, "amount": {"99.00"}, "date": {"2026-03-01"}},
			false, "viewer adds an expense"},
		{store.RoleViewer, viewer, "/income",
			url.Values{"label": {"Sneaky"}, "amount": {"99.00"}, "date": {"2026-03-01"}},
			false, "viewer adds income"},
		{store.RoleViewer, viewer, "/funds",
			url.Values{"name": {"Sneaky"}, "goal": {"1"}},
			false, "viewer creates a fund"},
		{store.RoleViewer, viewer, "/budgets",
			url.Values{"category": {"Sneaky"}, "limit": {"1"}},
			false, "viewer sets a category budget"},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			rig.loginAs(c.email)

			memberID := mustUserID(t, rig, c.email)
			hh, err := rig.store.ActiveHousehold(context.Background(), store.User{ID: memberID})
			if err != nil {
				t.Fatalf("household: %v", err)
			}
			if hh.Role != c.role {
				t.Fatalf("role is %q, want %q", hh.Role, c.role)
			}
			sc := store.Scope{HouseholdID: hh.ID, UserID: memberID}

			before := countRows(t, rig, sc)
			rig.post(c.target, c.form)
			after := countRows(t, rig, sc)

			changed := before != after
			if changed != c.allow {
				t.Errorf("%s: data changed=%v, want %v (before %v, after %v)",
					c.what, changed, c.allow, before, after)
			}
		})
	}
}

// TestViewerCannotMoveMoneyIntoSavings covers the one rule that is stricter than
// "editors write, viewers read": moving money in or out of a fund is the owner's
// alone, so an EDITOR must be refused here even though they may add an expense.
func TestEditorCannotMoveFunds(t *testing.T) {
	rig, hh, editor, _ := sharedRig(t)

	// Fund it as the owner first, so the withdrawal being refused cannot be
	// mistaken for the fund merely being empty.
	owner := store.Scope{HouseholdID: hh, UserID: rig.userID}
	if _, err := rig.store.Add(context.Background(), owner, store.NewTransaction{
		Kind: store.KindIncome, Label: "Salary", Amount: 100000, OccurredOn: "2026-03-01",
	}); err != nil {
		t.Fatalf("seed income: %v", err)
	}
	fundID, err := rig.store.CreateFund(context.Background(), owner, "Holiday", 50000, 0)
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}
	if err := rig.store.Deposit(context.Background(), owner, fundID, 20000, "2026-03-02"); err != nil {
		t.Fatalf("seed deposit: %v", err)
	}

	balanceOf := func() money.Cents {
		f, err := rig.store.FundByID(context.Background(), owner, fundID)
		if err != nil {
			t.Fatalf("fund: %v", err)
		}
		return f.Balance
	}
	before := balanceOf()

	rig.loginAs(editor)
	for _, action := range []string{"deposit", "withdraw", "close"} {
		rig.post(fmt.Sprintf("/funds/%d/%s", fundID, action), url.Values{"amount": {"50.00"}})
		if got := balanceOf(); got != before {
			t.Fatalf("editor %s changed the fund balance from %s to %s",
				action, before.Display(), got.Display())
		}
	}
}

// TestViewerSeesNoWriteControls checks the UI agrees with the middleware.
//
// Enforcement alone is not enough: a button that posts to a route the server
// refuses is a bug even though nothing is lost by it, because the user fills in
// a form and is then told they may not. The templates test the same
// Role.CanEditEntries method the routes do, and this asserts the result.
func TestViewerSeesNoWriteControls(t *testing.T) {
	rig, _, _, viewer := sharedRig(t)
	rig.loginAs(viewer)

	body := rig.do("GET", "/dashboard", nil).Body.String()
	for _, forbidden := range []string{`href="/income"`, `href="/expense"`, `action="/funds`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("dashboard offers %s to a viewer", forbidden)
		}
	}
	if !strings.Contains(body, `href="/transactions"`) {
		t.Error("dashboard should still offer a viewer the transactions list")
	}

	// And the page a viewer must not reach at all answers 403.
	if rec := rig.do("GET", "/expense", nil); rec.Code != http.StatusForbidden {
		t.Errorf("GET /expense as viewer returned %d, want 403", rec.Code)
	}
}

// TestHouseholdsAreIsolated is the invariant that matters most: two households
// must never see each other's money, however the request is shaped.
func TestHouseholdsAreIsolated(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// The owner's personal budget gets an expense.
	if _, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "PersonalSecret", Amount: 4200,
		OccurredOn: "2026-03-01",
	}); err != nil {
		t.Fatalf("seed personal expense: %v", err)
	}

	// Switching to a brand-new shared budget must show none of it.
	hh, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Flat 4B")
	if err != nil {
		t.Fatalf("create shared: %v", err)
	}
	rig.post("/household/switch", url.Values{"household_id": {fmt.Sprint(hh)}})

	body := rig.do("GET", "/transactions", nil).Body.String()
	if strings.Contains(body, "PersonalSecret") {
		t.Error("a shared budget shows an entry from the user's personal budget")
	}

	totals, err := rig.store.Totals(context.Background(),
		store.Scope{HouseholdID: hh, UserID: rig.userID}, "")
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Expense != 0 {
		t.Errorf("new shared budget already has %s of spending", totals.Expense.Display())
	}

	// And switching into a household the user is not a member of must fail.
	stranger, err := rig.store.CreateUser(context.Background(), "stranger@example.com", "hash")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	theirs, err := rig.store.CreateSharedHousehold(context.Background(), stranger, "Not Yours")
	if err != nil {
		t.Fatalf("create stranger household: %v", err)
	}
	if err := rig.store.SwitchHousehold(context.Background(), rig.userID, theirs); err == nil {
		t.Error("switched into a household the user does not belong to")
	}
}

// TestLastOwnerCannotBeRemoved guards the one structural rule SQL cannot express.
func TestLastOwnerCannotBeRemoved(t *testing.T) {
	rig, hh, _, _ := sharedRig(t)
	ctx := context.Background()

	if err := rig.store.RemoveMember(ctx, hh, rig.userID, rig.userID); !errors.Is(err, store.ErrLastOwner) {
		t.Errorf("removing the only owner gave %v, want ErrLastOwner", err)
	}
	if err := rig.store.SetRole(ctx, hh, rig.userID, rig.userID, store.RoleViewer); !errors.Is(err, store.ErrLastOwner) {
		t.Errorf("demoting the only owner gave %v, want ErrLastOwner", err)
	}

	// With a second owner in place, both become legal.
	second := rig.addMember(hh, "co-owner@example.com", store.RoleEditor)
	secondID := mustUserID(t, rig, second)
	if err := rig.store.SetRole(ctx, hh, rig.userID, secondID, store.RoleOwner); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := rig.store.SetRole(ctx, hh, rig.userID, rig.userID, store.RoleViewer); err != nil {
		t.Errorf("demoting one of two owners failed: %v", err)
	}
}

// TestRemovedMemberKeepsTheirEntries: removing somebody must not rewrite the
// household's history, because the rows belong to the household.
func TestRemovedMemberKeepsTheirEntries(t *testing.T) {
	rig, hh, editor, _ := sharedRig(t)
	ctx := context.Background()

	editorID := mustUserID(t, rig, editor)
	editorScope := store.Scope{HouseholdID: hh, UserID: editorID}
	if _, err := rig.store.Add(ctx, editorScope, store.NewTransaction{
		Kind: store.KindExpense, Label: "TheirShop", Amount: 3300, OccurredOn: "2026-03-01",
	}); err != nil {
		t.Fatalf("editor expense: %v", err)
	}

	owner := store.Scope{HouseholdID: hh, UserID: rig.userID}
	before, err := rig.store.Totals(ctx, owner, "")
	if err != nil {
		t.Fatalf("totals: %v", err)
	}

	if err := rig.store.RemoveMember(ctx, hh, rig.userID, editorID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	after, err := rig.store.Totals(ctx, owner, "")
	if err != nil {
		t.Fatalf("totals after: %v", err)
	}
	if before.Expense != after.Expense {
		t.Errorf("removing a member changed spending from %s to %s",
			before.Expense.Display(), after.Expense.Display())
	}
	if after.Expense != 3300 {
		t.Errorf("expected the entry to survive at $33.00, got %s", after.Expense.Display())
	}
}

// countRows is a cheap fingerprint of everything a write could have changed, so
// a permission test does not have to know which table an endpoint touches.
func countRows(t *testing.T, rig *testRig, sc store.Scope) [4]int {
	t.Helper()
	ctx := context.Background()

	txs, total, err := rig.store.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_ = txs

	funds, err := rig.store.ListFunds(ctx, sc)
	if err != nil {
		t.Fatalf("funds: %v", err)
	}
	buckets, err := rig.store.Buckets(ctx, sc, "2026-03")
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	budgets, err := rig.store.ListBudgets(ctx, sc, "2026-03")
	if err != nil {
		t.Fatalf("budgets: %v", err)
	}
	return [4]int{total, len(funds), len(buckets), len(budgets)}
}

func mustUserID(t *testing.T, rig *testRig, email string) int64 {
	t.Helper()
	u, _, err := rig.store.CredentialsFor(context.Background(), email)
	if err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	return u.ID
}

// ── the GET-delete bug ────────────────────────────────────────────────────────

// TestDeleteIsNotReachableByGET is the regression test for the old route
// GET /delete-transaction?id=N&type=expense, which any link prefetch, crawler,
// or <img src> on a visited page could fire.
func TestDeleteIsNotReachableByGET(t *testing.T) {
	rig := newRig(t)
	rig.login()

	essential := true
	id, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Food", Amount: 5000,
		OccurredOn: "2026-01-01", Essential: &essential,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The old URL shape must not exist at all.
	if rec := rig.do("GET", "/delete-transaction?id=1&type=expense", nil); rec.Code != http.StatusNotFound {
		t.Errorf("legacy delete URL returned %d, want 404", rec.Code)
	}

	// Nor may the new path be triggered with GET.
	rec := rig.do("GET", "/transactions/1/delete", nil)
	if rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
		t.Errorf("GET on the delete path returned %d; it must not be routable", rec.Code)
	}

	// The row is still there.
	if _, err := rig.store.ByID(context.Background(), rig.scope, id); err != nil {
		t.Errorf("transaction was destroyed by a GET: %v", err)
	}
}

func TestDeleteRequiresCSRFToken(t *testing.T) {
	rig := newRig(t)
	rig.login()

	essential := true
	id, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Food", Amount: 5000,
		OccurredOn: "2026-01-01", Essential: &essential,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	path := "/transactions/" + itoa(id) + "/delete"

	// No token at all.
	if rec := rig.do("POST", path, url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("POST without a token returned %d, want 403", rec.Code)
	}

	// A wrong token.
	if rec := rig.do("POST", path, url.Values{"csrf_token": {"not-the-real-token"}}); rec.Code != http.StatusForbidden {
		t.Errorf("POST with a bad token returned %d, want 403", rec.Code)
	}

	if _, err := rig.store.ByID(context.Background(), rig.scope, id); err != nil {
		t.Fatalf("row deleted despite failed CSRF checks: %v", err)
	}

	// The real token works.
	token := rig.csrf("/transactions")
	if rec := rig.do("POST", path, url.Values{"csrf_token": {token}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST with a valid token returned %d, want 303", rec.Code)
	}
	if _, err := rig.store.ByID(context.Background(), rig.scope, id); err == nil {
		t.Error("row survived a valid delete")
	}
}

// ── authentication ────────────────────────────────────────────────────────────

func TestProtectedRoutesRedirectWhenSignedOut(t *testing.T) {
	rig := newRig(t)

	for _, path := range []string{
		"/dashboard", "/transactions", "/transactions/new", "/transactions/export.csv",
		"/import", "/notifications",
	} {
		rec := rig.do("GET", path, nil)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s returned %d while signed out, want 303", path, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("%s redirected to %q, want /", path, got)
		}
	}
}

func TestBadLoginRerendersFormInsteadOf401Page(t *testing.T) {
	rig := newRig(t)
	token := rig.csrf("/")

	rec := rig.do("POST", "/auth", url.Values{
		"csrf_token": {token},
		"email":      {testEmail},
		"password":   {"wrong"},
	})

	body := rec.Body.String()
	// The old handler answered http.Error(w, "Invalid login", 401), a plain
	// text page with no form to retry from.
	if !strings.Contains(body, "<form") {
		t.Error("failed login should re-render the form")
	}
	if !strings.Contains(body, "do not match") {
		t.Errorf("failed login should explain itself, body was %q", truncate(body))
	}
	// The username is preserved so the user does not retype it.
	if !strings.Contains(body, `value="`+testEmail+`"`) {
		t.Error("failed login should preserve the email address")
	}
}

func TestLoginDoesNotRevealWhetherAUsernameExists(t *testing.T) {
	rig := newRig(t)

	// An address with no account is offered a signup, which is the wireframe's
	// behaviour and unavoidably reveals that it is unregistered. What must NOT
	// differ is the wrong-password message for two *existing* accounts, and the
	// signup offer must never appear for an address that does exist.
	unknown := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {"nobody-here@example.com"},
		"password":   {"whatever"},
	}).Body.String()

	if !strings.Contains(unknown, "doesn't have an account yet") {
		t.Errorf("an unknown address should be offered a signup, got %q", truncate(unknown))
	}

	known := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {testEmail},
		"password":   {"wrong"},
	}).Body.String()

	if strings.Contains(known, "doesn't have an account yet") {
		t.Error("an existing address must never be offered a signup")
	}
	if !strings.Contains(known, "do not match") {
		t.Errorf("wrong password should say so, got %q", extractError(known))
	}
}

// TestSignupRequiresTheConfirmStep covers the wireframe's two-step flow: an
// unknown address is asked whether to create an account, and only a submission
// carrying create=yes with a matching confirmation actually creates one.
func TestSignupRequiresTheConfirmStep(t *testing.T) {
	rig := newRig(t)

	// Step one: no account is created yet.
	body := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {"fresh@example.com"},
		"password":   {"longenough123"},
	}).Body.String()
	if !strings.Contains(body, "Confirm password") {
		t.Fatalf("expected the confirm step, got %q", truncate(body))
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "fresh@example.com"); exists {
		t.Fatal("the first submission must not create the account")
	}

	// A mismatched confirmation is refused.
	body = rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"create":     {"yes"},
		"email":      {"fresh@example.com"},
		"password":   {"longenough123"},
		"confirm":    {"somethingelse"},
	}).Body.String()
	if !strings.Contains(body, "do not match") {
		t.Errorf("mismatched confirmation should be refused, got %q", extractError(body))
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "fresh@example.com"); exists {
		t.Fatal("a mismatched confirmation must not create the account")
	}

	// A matching one creates it and signs the user straight in.
	rec := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"create":     {"yes"},
		"email":      {"fresh@example.com"},
		"password":   {"longenough123"},
		"confirm":    {"longenough123"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status %d -> %q, want 303 -> /dashboard", rec.Code, rec.Header().Get("Location"))
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "fresh@example.com"); !exists {
		t.Error("the account should now exist")
	}
}

func TestAuthRejectsAnythingThatIsNotAnEmailAddress(t *testing.T) {
	bad := []string{
		"kushith",     // a bare name, which a legacy row could actually hold
		"nope",
		"no@dot",      // no dot in the domain
		"a@b.c",       // single-character TLD
		"a@.com",      // empty first label
		"a@b.",        // trailing dot
		"a@b..com",    // empty label
		"@example.com",
		"user@",
		"a b@example.com",
		"two@@x.com",
		"a@-b.com",    // label starts with a hyphen
		"a@b.c1",      // digit in the TLD
	}
	for _, e := range bad {
		rig := newRig(t)
		rec := rig.do("POST", "/auth", url.Values{
			"csrf_token": {rig.csrf("/")},
			"email":      {e},
			"password":   {"longenough123"},
		})
		body := rec.Body.String()
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q returned %d, want 400", e, rec.Code)
		}
		if !strings.Contains(body, "valid email address") {
			t.Errorf("%q should be refused as an email, got %q", e, extractError(body))
		}
		// A malformed value must never reach the signup offer.
		if strings.Contains(body, "doesn't have an account yet") {
			t.Errorf("%q was offered an account despite not being an email", e)
		}
	}
}

func TestAuthAcceptsRealisticEmailAddresses(t *testing.T) {
	// The validator must not be so strict that it locks people out; these are all
	// legal addresses and none should be refused for their shape.
	for _, e := range []string{
		"a@b.co",
		"first.last@sub.example.co.uk",
		"user+tag@example.org",
		"x_y-z@my-domain.com",
	} {
		rig := newRig(t)
		body := rig.do("POST", "/auth", url.Values{
			"csrf_token": {rig.csrf("/")},
			"email":      {e},
			"password":   {"longenough123"},
		}).Body.String()
		if strings.Contains(body, "valid email address") {
			t.Errorf("%q was wrongly refused: %q", e, extractError(body))
		}
	}
}

// TestLoginPathAlsoValidatesTheEmail covers the case that prompted this: an
// account whose stored identifier is a bare name (migration 3 backfilled email
// from the old username column) must not be reachable by typing that name.
func TestLoginPathAlsoValidatesTheEmail(t *testing.T) {
	rig := newRig(t)

	// Create such a row directly, as the migration would have left it.
	if _, err := rig.store.CreateUser(context.Background(), "legacyname", "hash"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exists, err := rig.store.EmailExists(context.Background(), "legacyname")
	if err != nil || !exists {
		t.Fatalf("fixture should exist: %v %v", exists, err)
	}

	rec := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {"legacyname"},
		"password":   {"whatever"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "valid email address") {
		t.Errorf("a bare name must be refused even though an account matches it, got %q",
			extractError(rec.Body.String()))
	}
}

func TestLoginRateLimitEventuallyBlocks(t *testing.T) {
	rig := newRig(t)

	// store.RateMaxTries, not a local constant: the limiter moved into the
	// database so a restart could not clear a lockout, and the budget moved with
	// it. Reading it from the store is what keeps this test honest if it changes.
	var blocked bool
	for i := 0; i < store.RateMaxTries+3; i++ {
		body := rig.do("POST", "/auth", url.Values{
			"csrf_token": {rig.csrf("/")},
			"email":      {testEmail},
			"password":   {"wrong"},
		}).Body.String()
		if strings.Contains(body, "Too many failed attempts") {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Errorf("no rate limiting after %d failed attempts", store.RateMaxTries+3)
	}

	// A correct password is refused while the window is open, which is the
	// point of the limit.
	body := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {testEmail},
		"password":   {testPassword},
	}).Body.String()
	if !strings.Contains(body, "Too many failed attempts") {
		t.Error("rate limit should still apply to a correct password in the same window")
	}

	// The message has to say how long, in whole minutes. "Please wait a few
	// minutes" is the one thing a locked-out person cannot act on: they do not
	// know whether to wait or go away and come back.
	countdown := regexp.MustCompile(`Try again in (\d+) minutes?\.`).FindStringSubmatch(body)
	if countdown == nil {
		t.Fatalf("no countdown in the refusal: %q", extractError(body))
	}
	mins, _ := strconv.Atoi(countdown[1])
	if mins < 1 || mins > int(store.RateWindow/time.Minute) {
		t.Errorf("countdown says %d minutes, outside the %v window",
			mins, store.RateWindow)
	}
	if msg := extractError(body); strings.Contains(msg, "second") {
		t.Errorf("the countdown mentions seconds; it should be whole minutes only: %q", msg)
	}
}

// TestRetryPhraseRoundsUp: rounding down would tell somebody to come back at a
// moment that still refuses them, which earns a second helping of the same
// annoyance.
func TestRetryPhraseRoundsUp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "now"},
		{-time.Second, "now"},
		{30 * time.Second, "in 1 minute"},
		{time.Minute, "in 1 minute"},
		{61 * time.Second, "in 2 minutes"},
		{9*time.Minute + 40*time.Second, "in 10 minutes"},
		{10 * time.Minute, "in 10 minutes"},
	}
	for _, c := range cases {
		if got := retryPhrase(c.in); got != c.want {
			t.Errorf("retryPhrase(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSignupValidation(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		password string
		confirm  string
		wantMsg  string
	}{
		{"short password", "new@example.com", "short", "short", "at least 8"},
		{"mismatch", "new@example.com", "longenough123", "different123", "do not match"},
		{"whitespace password", "new@example.com", "         ", "         ", "whitespace"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			body := rig.do("POST", "/auth", url.Values{
				"csrf_token": {rig.csrf("/")},
				"create":     {"yes"},
				"email":      {tc.email},
				"password":   {tc.password},
				"confirm":    {tc.confirm},
			}).Body.String()
			if !strings.Contains(body, tc.wantMsg) {
				t.Errorf("want a message containing %q, got %q", tc.wantMsg, extractError(body))
			}
		})
	}
}

// ── input validation through the handler ──────────────────────────────────────

func TestNegativeAmountsAreRejectedWithAMessage(t *testing.T) {
	rig := newRig(t)
	rig.login()
	token := rig.csrf("/transactions/new")

	for _, amount := range []string{"-100", "0", "abc", "1.234", ""} {
		rec := rig.do("POST", "/transactions/new", url.Values{
			"csrf_token": {token},
			"kind":       {"income"},
			"label":      {"Salary"},
			"amount":     {amount},
			"date":       {"2026-01-01"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("amount %q returned %d, want 400", amount, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "greater than zero") {
			t.Errorf("amount %q: missing explanation, got %q", amount, extractError(rec.Body.String()))
		}
	}

	totals, err := rig.store.Totals(context.Background(), rig.scope, "")
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Income != 0 {
		t.Errorf("income = %s after only invalid submissions", totals.Income.Display())
	}
}

func TestValidSubmissionCreatesAndRedirects(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.do("POST", "/transactions/new", url.Values{
		"csrf_token": {rig.csrf("/transactions/new")},
		"kind":       {"expense"},
		"label":      {"Food"},
		"amount":     {"12.34"},
		"date":       {"2026-01-01"},
		"essential":  {"no"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %q", rec.Code, truncate(rec.Body.String()))
	}

	txs, total, err := rig.store.List(context.Background(), rig.scope, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("got %d transactions, want 1", total)
	}
	if txs[0].Amount != 1234 {
		t.Errorf("amount = %d cents, want 1234", txs[0].Amount)
	}
	if txs[0].Essential == nil || *txs[0].Essential {
		t.Errorf("essential = %v, want false", txs[0].Essential)
	}
}

// ── output escaping ───────────────────────────────────────────────────────────

// TestLabelsAreEscapedInChartData covers the old template.JS(...) usage, which
// injected chart data straight into a <script> body with escaping switched off.
func TestLabelsAreEscapedInChartData(t *testing.T) {
	rig := newRig(t)
	rig.login()

	essential := true
	_, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind:       store.KindExpense,
		Label:      `</script><script>alert(1)</script>`,
		Amount:     1000,
		OccurredOn: "2026-01-01",
		Essential:  &essential,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := rig.do("GET", "/dashboard", nil).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a transaction label was rendered as executable markup")
	}
	// The label should still be present, escaped.
	if !strings.Contains(body, "&lt;/script&gt;") && !strings.Contains(body, "\\u003c/script\\u003e") {
		t.Error("the label does not appear in escaped form; check the encoding path")
	}
}

// ── receipts ──────────────────────────────────────────────────────────────────

func TestReceiptOfAnotherUserIs404(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// A transaction belonging to a second user.
	other, err := rig.store.CreateUser(context.Background(), "other@example.com", "hash")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	essential := true
	otherHH, err := rig.store.ActiveHousehold(context.Background(), store.User{ID: other})
	if err != nil {
		t.Fatalf("other household: %v", err)
	}
	otherScope := store.Scope{HouseholdID: otherHH.ID, UserID: other}

	id, err := rig.store.Add(context.Background(), otherScope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Private", Amount: 100,
		OccurredOn: "2026-01-01", Essential: &essential,
		ReceiptPath: "/etc/passwd", ReceiptName: "receipt.png",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := rig.do("GET", "/transactions/"+itoa(id)+"/receipt", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("reading another user's receipt returned %d, want 404", rec.Code)
	}
}

func TestUploadsDirectoryIsNotBrowsable(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// The old router mounted no handler here, but the files sat in ./uploads
	// with predictable names. Confirm there is no route serving that tree.
	for _, path := range []string{"/uploads/", "/uploads/anything.png"} {
		if rec := rig.do("GET", path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, rec.Code)
		}
	}
}

// ── security headers ──────────────────────────────────────────────────────────

func TestSecurityHeadersOnRenderedPages(t *testing.T) {
	rig := newRig(t)
	rig.login()

	h := rig.do("GET", "/dashboard", nil).Header()
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors: %q", csp)
	}
}

func TestSessionCookieIsHttpOnly(t *testing.T) {
	rig := newRig(t)
	rig.login()

	var found bool
	for _, c := range rig.cookies {
		if c.Name == sessionName {
			found = true
			if !c.HttpOnly {
				t.Error("session cookie is not HttpOnly, so script can read it")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if !found {
		t.Fatal("no session cookie was set")
	}
}

// ── CSV export ────────────────────────────────────────────────────────────────

func TestCSVExportEscapesFormulas(t *testing.T) {
	rig := newRig(t)
	rig.login()

	essential := true
	_, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: `=1+1`, Amount: 1000,
		OccurredOn: "2026-01-01", Essential: &essential,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := rig.do("GET", "/transactions/export.csv", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	// A leading = would be evaluated as a formula when opened in a spreadsheet.
	if !strings.Contains(body, `'=1+1`) {
		t.Errorf("formula not neutralised, body:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

var errRE = regexp.MustCompile(`(?s)class="field-error"[^>]*>(.*?)</p>`)

func extractError(body string) string {
	m := errRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// ── the receipt loop, end to end ──────────────────────────────────────────────
//
// The bug these cover: /transactions/new accepted a ?receipt= parameter in the
// notification link the worker sends, and no handler read it. The form opened
// blank, the file was never attached, and the job's transaction_id stayed NULL
// forever. Two receipts sat orphaned in the real database because of it.

// waitingReceipt uploads a receipt and marks it processed-but-unread, which is
// the state the worker leaves one in when there is no OCR to read the amount.
func (r *testRig) waitingReceipt(name string) int64 {
	r.t.Helper()
	jobID, err := r.store.EnqueueReceipt(context.Background(), r.scope,
		"uploads/test/"+name, name)
	if err != nil {
		r.t.Fatalf("enqueue receipt: %v", err)
	}
	if err := r.store.CompleteReceiptJob(context.Background(), jobID, nil); err != nil {
		r.t.Fatalf("complete receipt job: %v", err)
	}
	return jobID
}

// TestReceiptLinkOpensAFormThatKnowsAboutIt is the fix stated directly: the link
// the notification sends must carry the receipt into the form.
func TestReceiptLinkOpensAFormThatKnowsAboutIt(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	rec := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, fmt.Sprintf(`name="receipt_job" value="%d"`, jobID)) {
		t.Error("the form does not carry the receipt id, so submitting would lose it")
	}
	if !strings.Contains(body, "lidl.png") {
		t.Error("the form does not say which receipt it is about")
	}
}

// TestEnteringDetailsAttachesTheReceipt walks the whole path the user walks.
func TestEnteringDetailsAttachesTheReceipt(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	rec := rig.post("/transactions/new", url.Values{
		"kind":        {"expense"},
		"label":       {"Groceries"},
		"amount":      {"31.40"},
		"date":        {"2026-08-01"},
		"receipt_job": {fmt.Sprint(jobID)},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// The receipt is consumed...
	if _, err := rig.store.UnattachedReceipt(context.Background(), rig.scope, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the receipt is still unattached after being used: %v", err)
	}

	// ...and the expense carries it.
	var path, name string
	err := rig.db.QueryRow(`
		SELECT IFNULL(receipt_path,''), IFNULL(receipt_name,'')
		FROM transactions WHERE label = 'Groceries'`).Scan(&path, &name)
	if err != nil {
		t.Fatalf("read the new expense: %v", err)
	}
	if path == "" || name != "lidl.png" {
		t.Errorf("the expense has no receipt: path=%q name=%q", path, name)
	}
}

// TestTheReceiptSurvivesAValidationError is the detail that makes the fix usable.
// Mistyping the amount must not silently detach the receipt.
func TestTheReceiptSurvivesAValidationError(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	rec := rig.post("/transactions/new", url.Values{
		"kind":        {"expense"},
		"label":       {"Groceries"},
		"amount":      {"not a number"},
		"date":        {"2026-08-01"},
		"receipt_job": {fmt.Sprint(jobID)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad amount should re-render the form, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`name="receipt_job" value="%d"`, jobID)) {
		t.Error("the receipt was dropped when the form came back with an error")
	}
}

// TestAnAlreadyUsedReceiptIsRefused covers the double-submit: back button,
// double-tapped notification, or the same link opened in two tabs.
func TestAnAlreadyUsedReceiptIsRefused(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	form := url.Values{
		"kind":        {"expense"},
		"label":       {"Groceries"},
		"amount":      {"31.40"},
		"date":        {"2026-08-01"},
		"receipt_job": {fmt.Sprint(jobID)},
	}
	if rec := rig.post("/transactions/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("first submit: %d", rec.Code)
	}

	rec := rig.post("/transactions/new", url.Values{
		"kind":        {"expense"},
		"label":       {"Groceries again"},
		"amount":      {"31.40"},
		"date":        {"2026-08-01"},
		"receipt_job": {fmt.Sprint(jobID)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a reused receipt should be refused, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already been used") {
		t.Error("the refusal does not explain itself")
	}

	// And no second expense was created.
	var n int
	if err := rig.db.QueryRow(
		`SELECT COUNT(*) FROM transactions WHERE label LIKE 'Groceries%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d expenses created, want 1", n)
	}
}

// TestAnotherHouseholdsReceiptIsNotOffered is the same IDOR check at the HTTP
// layer, since the id travels in a URL a curious user can edit.
func TestAnotherHouseholdsReceiptIsNotOffered(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// A receipt belonging to somebody else's personal budget.
	other := rig.addMember(rig.scope.HouseholdID, "outsider@example.com", store.RoleViewer)
	var otherUID int64
	if err := rig.db.QueryRow(`SELECT id FROM users WHERE email = ?`, other).Scan(&otherUID); err != nil {
		t.Fatal(err)
	}
	var personal int64
	if err := rig.db.QueryRow(
		`SELECT id FROM households WHERE personal_for = ?`, otherUID).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	jobID, err := rig.store.EnqueueReceipt(context.Background(),
		store.Scope{HouseholdID: personal, UserID: otherUID}, "uploads/x/theirs.png", "theirs.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := rig.store.CompleteReceiptJob(context.Background(), jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rec := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "theirs.png") {
		t.Error("another household's receipt filename is shown")
	}
	if strings.Contains(body, `name="receipt_job"`) {
		t.Error("another household's receipt is offered for attaching")
	}

	// Posting it anyway must not attach it.
	if rec := rig.post("/transactions/new", url.Values{
		"kind": {"expense"}, "label": {"Nice try"}, "amount": {"5.00"},
		"date": {"2026-08-01"}, "receipt_job": {fmt.Sprint(jobID)},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("posting another household's receipt returned %d", rec.Code)
	}
}

// TestWaitingReceiptsAreReachableWithoutTheNotification: notifications get
// dismissed, cleared on another device, or scrolled past. The file must not
// become unreachable when that happens.
func TestWaitingReceiptsAreReachableWithoutTheNotification(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	rec := rig.do("GET", "/expense", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "lidl.png") {
		t.Error("Add Expense does not mention the receipt waiting for details")
	}
	if !strings.Contains(body, fmt.Sprintf("receipt=%d", jobID)) {
		t.Error("Add Expense offers no link to finish the waiting receipt")
	}
}

// TestAnEnteredReceiptLeavesTheWaitingList closes the loop from the user's side:
// once entered, it stops being nagged about.
func TestAnEnteredReceiptLeavesTheWaitingList(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	if rec := rig.post("/transactions/new", url.Values{
		"kind": {"expense"}, "label": {"Groceries"}, "amount": {"31.40"},
		"date": {"2026-08-01"}, "receipt_job": {fmt.Sprint(jobID)},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("submit: %d", rec.Code)
	}

	rec := rig.do("GET", "/expense", nil)
	if strings.Contains(rec.Body.String(), "lidl.png") {
		t.Error("an entered receipt is still listed as waiting")
	}
}

// TestIncomeIgnoresAReceiptParameter: income has no receipt field, so the
// parameter must not smuggle one in.
func TestIncomeIgnoresAReceiptParameter(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("lidl.png")

	rec := rig.do("GET", fmt.Sprintf("/transactions/new?type=income&receipt=%d", jobID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `name="receipt_job"`) {
		t.Error("the income form offers to attach a receipt")
	}
}

// ── password reset, end to end through HTTP ───────────────────────────────────

// TestForgotDoesNotRevealWhetherAnAddressExists is the security property of the
// endpoint. A different message, status or shape for a known address would turn
// the page into a way of enumerating who has an account.
func TestForgotDoesNotRevealWhetherAnAddressExists(t *testing.T) {
	rig := newRig(t)

	known := rig.do("POST", "/forgot", url.Values{
		"email":      {testEmail},
		"csrf_token": {rig.csrf("/forgot")},
	})
	unknown := rig.do("POST", "/forgot", url.Values{
		"email":      {"nobody@example.com"},
		"csrf_token": {rig.csrf("/forgot")},
	})

	if known.Code != unknown.Code {
		t.Errorf("status differs: known %d, unknown %d", known.Code, unknown.Code)
	}
	// Compare the pages with the address itself removed, since the page echoes it.
	strip := func(rec *httptest.ResponseRecorder, email string) string {
		return strings.ReplaceAll(rec.Body.String(), email, "ADDRESS")
	}
	if strip(known, testEmail) != strip(unknown, "nobody@example.com") {
		t.Error("the response differs between a known and an unknown address")
	}
}

func TestResetLinkSetsANewPassword(t *testing.T) {
	rig := newRig(t)

	token, err := rig.store.CreateReset(context.Background(), rig.userID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	// The form appears for a valid token, and names the account.
	form := rig.do("GET", "/reset?token="+token, nil)
	if form.Code != http.StatusOK {
		t.Fatalf("reset form: %d", form.Code)
	}
	if !strings.Contains(form.Body.String(), testEmail) {
		t.Error("the reset form does not say which account it is for")
	}

	rec := rig.do("POST", "/reset", url.Values{
		"token":      {token},
		"password":   {"a-brand-new-password"},
		"confirm":    {"a-brand-new-password"},
		"csrf_token": {rig.csrf("/reset?token=" + token)},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset submit: %d — %s", rec.Code, rec.Body.String())
	}

	// The old password no longer works and the new one does.
	_, hash, err := rig.store.CredentialsFor(context.Background(), testEmail)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)) == nil {
		t.Error("the old password still works")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("a-brand-new-password")) != nil {
		t.Error("the new password does not work")
	}
}

// TestResetRevokesEverySession is the reason a reset differs from a change: the
// usual reason for resetting is that somebody else may have had the old password.
func TestResetRevokesEverySession(t *testing.T) {
	rig := newRig(t)
	rig.login()

	var before int
	if err := rig.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`,
		rig.userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("no session to revoke")
	}

	token, _ := rig.store.CreateReset(context.Background(), rig.userID)
	rec := rig.do("POST", "/reset", url.Values{
		"token":      {token},
		"password":   {"a-brand-new-password"},
		"confirm":    {"a-brand-new-password"},
		"csrf_token": {rig.csrf("/reset?token=" + token)},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", rec.Code)
	}

	var after int
	rig.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, rig.userID).Scan(&after)
	if after != 0 {
		t.Errorf("%d session(s) survived a password reset", after)
	}
	// And the browser that was signed in is now bounced to the login page.
	if got := rig.do("GET", "/dashboard", nil); got.Code != http.StatusSeeOther {
		t.Errorf("the old session still reaches the dashboard: %d", got.Code)
	}
}

func TestAnExpiredResetLinkExplainsItself(t *testing.T) {
	rig := newRig(t)

	token, _ := rig.store.CreateReset(context.Background(), rig.userID)
	if _, err := rig.db.Exec(
		`UPDATE password_resets SET expires_at = datetime('now','-1 minute') WHERE token = ?`,
		token); err != nil {
		t.Fatal(err)
	}

	rec := rig.do("GET", "/reset?token="+token, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no longer works") {
		t.Error("the page does not explain that the link expired")
	}
	if strings.Contains(body, `name="password"`) {
		t.Error("a form is shown that could never succeed")
	}
}

func TestResetTokenCannotBeUsedTwiceOverHTTP(t *testing.T) {
	rig := newRig(t)
	token, _ := rig.store.CreateReset(context.Background(), rig.userID)

	form := url.Values{
		"token":    {token},
		"password": {"a-brand-new-password"},
		"confirm":  {"a-brand-new-password"},
	}
	csrf := rig.csrf("/reset?token=" + token)
	form.Set("csrf_token", csrf)
	if rec := rig.do("POST", "/reset", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", rec.Code)
	}

	// Reuse the same CSRF token rather than fetching the page again: once the
	// link is spent, GET /reset is a 400 with no form on it, so there is nothing
	// to read a fresh token from. A real browser would be in the same position,
	// posting from a page it already had open.
	form.Set("csrf_token", csrf)
	form.Set("password", "yet-another-password")
	form.Set("confirm", "yet-another-password")
	rec := rig.do("POST", "/reset", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("the same link worked twice: %d", rec.Code)
	}
}

func TestResetRefusesAMismatchedConfirmation(t *testing.T) {
	rig := newRig(t)
	token, _ := rig.store.CreateReset(context.Background(), rig.userID)

	rec := rig.do("POST", "/reset", url.Values{
		"token":      {token},
		"password":   {"a-brand-new-password"},
		"confirm":    {"a-brand-new-passwrod"},
		"csrf_token": {rig.csrf("/reset?token=" + token)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "do not match") {
		t.Error("the mismatch is not explained")
	}
	// The token is still usable, since nothing was changed.
	if _, err := rig.store.ResetUser(context.Background(), token); err != nil {
		t.Error("a typo in the confirmation burned the token")
	}
}

// ── changing your own password ────────────────────────────────────────────────

func TestChangePasswordKeepsThisDeviceAndDropsOthers(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// A second device.
	other := newRigSession(t, rig)

	rec := rig.post("/password", url.Values{
		"current":  {testPassword},
		"password": {"a-brand-new-password"},
		"confirm":  {"a-brand-new-password"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("change: %d — %s", rec.Code, rec.Body.String())
	}

	// This session still works...
	if got := rig.do("GET", "/dashboard", nil); got.Code != http.StatusOK {
		t.Errorf("the device that changed the password was signed out: %d", got.Code)
	}
	// ...and the other one does not.
	var alive int
	rig.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, other).Scan(&alive)
	if alive != 0 {
		t.Error("the other device is still signed in")
	}
}

func TestChangePasswordNeedsTheCurrentOne(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.post("/password", url.Values{
		"current":  {"not-the-password"},
		"password": {"a-brand-new-password"},
		"confirm":  {"a-brand-new-password"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "current password") {
		t.Error("the refusal does not say what was wrong")
	}
	_, hash, _ := rig.store.CredentialsFor(context.Background(), testEmail)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)) != nil {
		t.Error("the password changed anyway")
	}
}

func TestChangePasswordRefusesTheSamePassword(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.post("/password", url.Values{
		"current":  {testPassword},
		"password": {testPassword},
		"confirm":  {testPassword},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already have") {
		t.Error("the refusal does not explain itself")
	}
}

// ── invitations, over HTTP ────────────────────────────────────────────────────

// TestExpiredInvitationSaysSoRatherThanVanishing distinguishes the two cases the
// recipient could be in.
func TestExpiredInvitationSaysSo(t *testing.T) {
	rig := newRig(t)
	rig.login()

	shared, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Flat 4B")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.store.InviteMember(context.Background(), shared, rig.userID,
		"guest@example.com", store.RoleEditor); err != nil {
		t.Fatal(err)
	}

	// The guest signs in.
	guest := newRig(t)
	_ = guest
	hash, _ := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	guestID, err := rig.store.CreateUser(context.Background(), "guest@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	invites, _ := rig.store.InvitesFor(context.Background(), guestID, "guest@example.com")
	if len(invites) != 1 {
		t.Fatalf("the guest has %d invitations", len(invites))
	}
	if err := rig.store.TestOnlyExpireInvite(context.Background(), invites[0].ID); err != nil {
		t.Fatal(err)
	}

	rig.loginAs("guest@example.com")
	rec := rig.post(fmt.Sprintf("/invites/%d/accept", invites[0].ID), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("accept: %d", rec.Code)
	}
	// The flash is on the next page.
	next := rig.do("GET", "/dashboard", nil)
	if !strings.Contains(next.Body.String(), "expired") {
		t.Error("the guest is not told the invitation expired")
	}
}

// ── optimistic concurrency, over HTTP ─────────────────────────────────────────

func TestEditFormCarriesTheVersion(t *testing.T) {
	rig := newRig(t)
	rig.login()

	id := rig.addExpenseVia(t, "Rent", "1000.00")
	rec := rig.do("GET", fmt.Sprintf("/transactions/%d/edit", id), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit form: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="version" value="1"`) {
		t.Error("the edit form does not carry the row version")
	}
}

// TestAConflictIsExplainedAndRecoverable: the page has to say what happened and
// let the user try again, or it is a dead end.
func TestAConflictIsExplainedAndRecoverable(t *testing.T) {
	rig := newRig(t)
	rig.login()

	id := rig.addExpenseVia(t, "Rent", "1000.00")

	// Somebody else saves first, bumping the version to 2.
	if err := rig.store.Update(context.Background(), rig.scope, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Rent (them)", Amount: 110000,
		OccurredOn: "2026-08-01", Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Our form still holds version 1.
	rec := rig.post(fmt.Sprintf("/transactions/%d/edit", id), url.Values{
		"kind":    {"expense"},
		"label":   {"Rent (us)"},
		"amount":  {"1200.00"},
		"date":    {"2026-08-01"},
		"version": {"1"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a conflict", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Somebody else changed this entry") {
		t.Error("the conflict is not explained")
	}
	if !strings.Contains(body, "Rent (them)") {
		t.Error("the page does not show what the other person saved")
	}
	if !strings.Contains(body, `name="version" value="2"`) {
		t.Error("the form is not re-armed with the new version, so retrying would fail again")
	}
	if !strings.Contains(body, "Rent (us)") {
		t.Error("the user's own typing was thrown away")
	}

	// Retrying with the version the page now carries succeeds.
	retry := rig.post(fmt.Sprintf("/transactions/%d/edit", id), url.Values{
		"kind":    {"expense"},
		"label":   {"Rent (us)"},
		"amount":  {"1200.00"},
		"date":    {"2026-08-01"},
		"version": {"2"},
	})
	if retry.Code != http.StatusSeeOther {
		t.Errorf("the retry failed: %d", retry.Code)
	}
}

// ── ownership transfer, over HTTP ─────────────────────────────────────────────

func TestTransferOwnershipRoute(t *testing.T) {
	rig := newRig(t)
	rig.login()

	shared, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Flat 4B")
	if err != nil {
		t.Fatal(err)
	}
	// household_id, not household: the handler reads household_id, so the wrong
	// name here was a silent 400 and every assertion below ran against the
	// personal budget instead of the shared one it meant to test.
	rig.post("/household/switch", url.Values{"household_id": {fmt.Sprint(shared)}})
	email := rig.addMember(shared, "guest@example.com", store.RoleEditor)

	var guestID int64
	if err := rig.db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&guestID); err != nil {
		t.Fatal(err)
	}

	rec := rig.post(fmt.Sprintf("/household/members/%d/transfer", guestID), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("transfer: %d", rec.Code)
	}

	var mine, theirs string
	rig.db.QueryRow(`SELECT role FROM household_members WHERE household_id=? AND user_id=?`,
		shared, rig.userID).Scan(&mine)
	rig.db.QueryRow(`SELECT role FROM household_members WHERE household_id=? AND user_id=?`,
		shared, guestID).Scan(&theirs)
	if theirs != "owner" || mine != "editor" {
		t.Errorf("after the transfer: me=%q them=%q, want editor/owner", mine, theirs)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// newRigSession signs the same account in a second time and returns the session
// id, standing in for another device.
func newRigSession(t *testing.T, rig *testRig) string {
	t.Helper()
	id, err := rig.store.CreateSession(context.Background(), rig.userID, "Another device")
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	return id
}

// addExpenseVia posts the add form and returns the new row's id.
func (r *testRig) addExpenseVia(t *testing.T, label, amount string) int64 {
	t.Helper()
	rec := r.post("/transactions/new", url.Values{
		"kind":   {"expense"},
		"label":  {label},
		"amount": {amount},
		"date":   {"2026-08-01"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add expense: %d — %s", rec.Code, rec.Body.String())
	}
	var id int64
	if err := r.db.QueryRow(`SELECT id FROM transactions WHERE label = ?`, label).Scan(&id); err != nil {
		t.Fatalf("find the new expense: %v", err)
	}
	return id
}

// ── discarding an unwanted upload ─────────────────────────────────────────────

func TestDiscardClearsTheWaitingListAndDeletesTheFile(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// A real file on disk, so the handler's delete has something to remove.
	dir := filepath.Join(rig.uploadDir, fmt.Sprint(rig.userID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "dupe.png")
	if err := os.WriteFile(file, []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stored paths are forward-slashed and include the upload directory, which
	// is what saveReceipt writes and what receiptFile expects to convert back.
	jobID, err := rig.store.EnqueueReceipt(context.Background(), rig.scope,
		filepath.ToSlash(file), "dupe.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := rig.store.CompleteReceiptJob(context.Background(), jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if body := rig.do("GET", "/expense", nil).Body.String(); !strings.Contains(body, "dupe.png") {
		t.Fatal("the receipt is not on the waiting list to begin with")
	}

	rec := rig.post(fmt.Sprintf("/receipts/%d/discard", jobID), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if body := rig.do("GET", "/expense", nil).Body.String(); strings.Contains(body, "dupe.png") {
		t.Error("a discarded receipt is still listed")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the uploaded file was left on disk")
	}
}

// TestDiscardIsPostOnly: a GET-able discard would be reachable by a prefetching
// browser or an <img> tag on any page the user visits.
func TestDiscardIsPostOnly(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("dupe.png")

	if rec := rig.do("GET", fmt.Sprintf("/receipts/%d/discard", jobID), nil); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET on discard returned %d", rec.Code)
	}
	if _, err := rig.store.UnattachedReceipt(context.Background(), rig.scope, jobID); err != nil {
		t.Errorf("the receipt was discarded by a GET: %v", err)
	}
}

// ── the page must not contradict the mailer ───────────────────────────────────

// TestHouseholdPageTellsTheTruthAboutEmail covers the bug where the page stated
// in two hardcoded places that nothing is emailed, next to a flash message
// saying an email had failed to send. Both could not be right.
func TestHouseholdPageTellsTheTruthAboutEmail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mail       bool
		wantSaid   string
		wantAbsent string
	}{
		{"no mail server", false, "Nothing is emailed", "They get an email"},
		{"mail configured", true, "They get an email", "Nothing is emailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRigWithMail(t, tc.mail)
			rig.login()

			// The invite panel only exists on a shared budget: a personal one has
			// nobody to invite, so the notice under test would not render at all.
			shared, err := rig.store.CreateSharedHousehold(context.Background(),
				rig.userID, "Flat 4B")
			if err != nil {
				t.Fatalf("create shared: %v", err)
			}
			rig.post("/household/switch", url.Values{"household_id": {fmt.Sprint(shared)}})

			body := rig.do("GET", "/household", nil).Body.String()
			if !strings.Contains(body, tc.wantSaid) {
				t.Errorf("page does not say %q", tc.wantSaid)
			}
			if strings.Contains(body, tc.wantAbsent) {
				t.Errorf("page still says %q when it should not", tc.wantAbsent)
			}
		})
	}
}

// ── double submit ─────────────────────────────────────────────────────────────

// TestDoubleSubmitRecordsTheMoneyOnce is the whole point: a second identical
// submission must not create a second expense.
func TestDoubleSubmitRecordsTheMoneyOnce(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// Take the token the real form would carry.
	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	m := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the Add Expense form carries no one-time token")
	}
	form := url.Values{
		"label": {"Groceries"}, "amount": {"31.40"}, "date": {"2026-08-01"},
		"form_token": {m[1]},
	}

	if rec := rig.post("/expense", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("first submit: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := rig.post("/expense", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("second submit should redirect, not error: %d", rec.Code)
	}

	var n int
	if err := rig.db.QueryRow(
		`SELECT COUNT(*) FROM transactions WHERE label = 'Groceries'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d expenses recorded from one form submitted twice, want 1", n)
	}
}

// TestValidationFailureDoesNotBurnTheToken: correcting a typo must still work.
func TestValidationFailureDoesNotBurnTheToken(t *testing.T) {
	rig := newRig(t)
	rig.login()

	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	m := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no token in the form")
	}

	bad := url.Values{"label": {"Groceries"}, "amount": {"not a number"},
		"date": {"2026-08-01"}, "form_token": {m[1]}}
	if rec := rig.post("/expense", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad amount: %d", rec.Code)
	}

	good := url.Values{"label": {"Groceries"}, "amount": {"31.40"},
		"date": {"2026-08-01"}, "form_token": {m[1]}}
	if rec := rig.post("/expense", good); rec.Code != http.StatusSeeOther {
		t.Fatalf("the corrected submit was refused: %d — %s", rec.Code, rec.Body.String())
	}
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE label='Groceries'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d expenses, want 1", n)
	}
}

// TestActivityAnswersWhoDeletedIt is the user-facing half of the audit log.
func TestActivityAnswersWhoDeletedIt(t *testing.T) {
	rig := newRig(t)
	rig.login()

	shared, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Flat 4B")
	if err != nil {
		t.Fatal(err)
	}
	rig.post("/household/switch", url.Values{"household_id": {fmt.Sprint(shared)}})

	id := rig.addExpenseVia(t, "Rent", "900.00")
	if rec := rig.post(fmt.Sprintf("/transactions/%d/delete", id), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}

	body := rig.do("GET", "/household", nil).Body.String()
	if !strings.Contains(body, "Recent activity") {
		t.Fatal("the household page shows no activity section")
	}

	// Scope the assertions to the activity list. Searching a whole HTML page for
	// a short word finds it somewhere almost every time — the rate-limit test
	// was already caught out by "second" matching btn-auth-secondary.
	activity := regexp.MustCompile(`(?s)<ul class="activity">(.*?)</ul>`).FindStringSubmatch(body)
	if activity == nil {
		t.Fatal("no activity list on the page")
	}
	list := activity[1]
	if !strings.Contains(list, "deleted") || !strings.Contains(list, "Rent") {
		t.Errorf("the history does not say the rent entry was deleted: %q", list)
	}
	if !strings.Contains(list, testEmail) && !strings.Contains(list, "Tester") &&
		!strings.Contains(list, "tester") {
		t.Errorf("the history does not say who did it: %q", list)
	}
}

// ── the sharing page's recent activity ────────────────────────────────────────

// TestRecentActivityIsCappedAtThree: the list answers "what just happened here?",
// so it shows the newest three and no more. The audit table still holds
// everything; only the view is capped.
func TestRecentActivityIsCappedAtThree(t *testing.T) {
	rig := newRig(t)
	rig.login()

	for i := 0; i < 6; i++ {
		rig.addExpenseVia(t, fmt.Sprintf("Coffee %d", i), "3.50")
	}

	body := rig.do("GET", "/household", nil).Body.String()

	// Scoped to the activity list rather than searched for across the page, for
	// the reason the audit test already gives: a short string matches somewhere
	// in a full HTML document almost every time.
	found := regexp.MustCompile(`(?s)<ul class="activity">(.*?)</ul>`).FindStringSubmatch(body)
	if found == nil {
		t.Fatal("no activity list on the page")
	}
	if n := strings.Count(found[1], "activity-who"); n != 3 {
		t.Errorf("activity list shows %d entries, want 3", n)
	}
}

// ── signing up from the sign-in page ──────────────────────────────────────────

// TestUnknownEmailIsAnsweredInRedOnThePage: an address with no account is told
// so in the page's own red banner, with the password step underneath it. No
// dialog and no second page: the answer belongs where the button was.
func TestUnknownEmailIsAnsweredInRedOnThePage(t *testing.T) {
	rig := newRig(t)

	body := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"email":      {"nobody@example.com"},
		"password":   {"longenough123"},
	}).Body.String()

	if !strings.Contains(body, `class="auth-error"`) {
		t.Errorf("the answer should be the page's red banner: %q", truncate(body))
	}
	if !strings.Contains(body, "doesn't have an account yet") {
		t.Error("the banner does not say the address has no account")
	}
	if !strings.Contains(body, "Confirm password") {
		t.Error("the password step should be on the page, ready to fill in")
	}
	// The same information used to arrive in a modal. It must not come back:
	// the signed-out page has no other dialog, so any <dialog> here is this one.
	if strings.Contains(body, "<dialog") {
		t.Error("this flow should not open a dialog")
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "nobody@example.com"); exists {
		t.Fatal("offering to create the account must not create it")
	}

	// A mismatch replaces the banner's text with the reason, in the same place,
	// and leaves the fields on screen to be corrected.
	body = rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")},
		"create":     {"yes"},
		"email":      {"nobody@example.com"},
		"password":   {"longenough123"},
		"confirm":    {"mismatch12345"},
	}).Body.String()

	if !strings.Contains(body, "do not match") {
		t.Errorf("a mismatch should be explained in the banner: %q", extractError(body))
	}
	if !strings.Contains(body, "Confirm password") {
		t.Error("the password step should still be on screen after a mismatch")
	}
}
