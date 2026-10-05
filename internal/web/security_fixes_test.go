package web

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestPasswordChangeLockoutHoldsEvenForTheRightPassword: once locked out, the
// correct current password must be refused as well, or the limit stops nothing.
func TestPasswordChangeLockoutHoldsEvenForTheRightPassword(t *testing.T) {
	rig := newRig(t)
	rig.login()

	for i := 0; i < store.RateMaxTries; i++ {
		rig.post("/password", url.Values{
			"current": {"wrong-guess"}, "password": {"another-password"}, "confirm": {"another-password"},
		})
	}
	rec := rig.post("/password", url.Values{
		"current": {testPassword}, "password": {"another-password"}, "confirm": {"another-password"},
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the right password got through a lockout: status %d", rec.Code)
	}
	_, hash, _ := rig.store.CredentialsFor(context.Background(), testEmail)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)) != nil {
		t.Fatal("the password was changed during a lockout")
	}
}

// TestSignupsPerAddressAreLimited: successful sign-ups count, not only refused ones.
func TestSignupsPerAddressAreLimited(t *testing.T) {
	rig := newRig(t)
	signup := func(i int) int {
		rig.cookies = nil
		return rig.do("POST", "/register", url.Values{
			"csrf_token": {rig.csrf("/register")},
			"email":      {fmt.Sprintf("user%d@example.com", i)},
			"password":   {"longenough123"},
			"confirm":    {"longenough123"},
		}).Code
	}
	for i := 0; i < int(store.SignupLimit.Max); i++ {
		if code := signup(i); code != http.StatusSeeOther {
			t.Fatalf("sign-up %d refused early: %d", i, code)
		}
	}
	if code := signup(99); code != http.StatusTooManyRequests {
		t.Fatalf("sign-up over the limit: status %d, want 429", code)
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "user99@example.com"); exists {
		t.Error("the account over the limit was created")
	}
}

// TestInvitationsToOneAddressAreLimited: new invitations and resends together
// may only email one address a few times a day.
func TestInvitationsToOneAddressAreLimited(t *testing.T) {
	rig := newRig(t)
	rig.login()
	if rec := rig.post("/household", url.Values{"name": {"Flat"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create household: %d", rec.Code)
	}
	if rec := rig.post("/household/invite", url.Values{"email": {"target@example.com"}, "role": {"viewer"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("invite: %d", rec.Code)
	}
	hh, err := rig.store.ActiveHousehold(context.Background(), store.User{ID: rig.userID})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := rig.store.PendingInvites(context.Background(), hh.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending invites: %v %v", pending, err)
	}
	resend := fmt.Sprintf("/household/invites/%d/resend", pending[0].ID)

	// The first invitation counted once; resends use up the rest.
	for i := 1; i < int(store.InviteRecipientLimit.Max); i++ {
		if rec := rig.post(resend, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("resend %d: %d", i, rec.Code)
		}
	}

	// Shorten the expiry so a resend that slipped through would be visible.
	rig.db.Exec(`UPDATE household_invites SET expires_at = datetime('now', '+1 hour')`)
	rig.post(resend, nil)

	var extended bool
	rig.db.QueryRow(`SELECT expires_at > datetime('now', '+2 hours') FROM household_invites`).Scan(&extended)
	if extended {
		t.Error("a resend over the limit still went through")
	}
}

// TestTokensAreStoredHashed: neither a session token nor a reset token may be
// usable straight out of the database.
func TestTokensAreStoredHashed(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	tok, err := rig.store.CreateSession(ctx, rig.userID, "x")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, tok).Scan(&n)
	if n != 0 {
		t.Error("the session token is stored as-is")
	}
	if _, err := rig.store.SessionUser(ctx, store.TokenHash(tok)); err == nil {
		t.Error("the stored hash works as a session token")
	}
	if _, err := rig.store.SessionUser(ctx, tok); err != nil {
		t.Errorf("the token no longer resolves: %v", err)
	}

	reset, err := rig.store.CreateReset(ctx, rig.userID)
	if err != nil {
		t.Fatal(err)
	}
	rig.db.QueryRow(`SELECT COUNT(*) FROM password_resets WHERE token = ?`, reset).Scan(&n)
	if n != 0 {
		t.Error("the reset token is stored as-is")
	}
	if _, err := rig.store.ResetUser(ctx, store.TokenHash(reset)); err == nil {
		t.Error("the stored hash works as a reset token")
	}
	if _, err := rig.store.ResetUser(ctx, reset); err != nil {
		t.Errorf("the reset token no longer resolves: %v", err)
	}
}

// TestSignInWithAnUnreadableCookieNeedsAToken: a cookie that cannot be decoded
// used to skip the CSRF check on /auth altogether.
func TestSignInWithAnUnreadableCookieNeedsAToken(t *testing.T) {
	rig := newRig(t)
	req := rig.request("POST", "/auth", url.Values{"email": {testEmail}, "password": {testPassword}})
	req.AddCookie(&http.Cookie{Name: sessionName, Value: "garbage"})
	if rec := rig.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("forged sign-in with a bad cookie: status %d, want 403", rec.Code)
	}
}

// TestNoInlineScriptsSoCSPCanForbidThem: the policy no longer allows inline
// script, so any template that reintroduces one would silently break.
func TestNoInlineScriptsSoCSPCanForbidThem(t *testing.T) {
	inline := regexp.MustCompile(`<script(\s[^>]*)?>\s*[^<\s]|\son[a-z]+="|javascript:`)
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(templateFS, p)
		// Template comments may describe the old markup.
		body := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(b), "")
		if m := inline.FindString(body); m != "" {
			t.Errorf("%s has inline script %q", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	rig := newRig(t)
	csp := rig.do("GET", "/", nil).Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP still allows inline script: %s", csp)
	}
}

// TestSecurityHeadersOnEveryResponse: an error page or a static file is still
// a page of this site and needs the same protection.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	rig := newRig(t)
	for _, target := range []string{"/no-such-page", "/static/ui.js", "/healthz"} {
		h := rig.do("GET", target, nil).Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s is missing security headers: %v", target, h)
		}
	}
}

// TestPublicFormsHaveABodyLimit: /forgot and /auth are reachable by anybody, so
// they must not accept a receipt-sized body.
func TestPublicFormsHaveABodyLimit(t *testing.T) {
	rig := newRig(t)
	big := strings.Repeat("a", formBodyLimit+10)
	rec := rig.do("POST", "/forgot", url.Values{"email": {big}})
	if rec.Code == http.StatusOK {
		t.Errorf("a %d-byte body to /forgot was accepted", len(big))
	}
}

func TestCSVSafeLooksPastLeadingSpaces(t *testing.T) {
	for in, want := range map[string]string{
		" =cmd|'/c calc'!A1": "' =cmd|'/c calc'!A1",
		"=1+1":               "'=1+1",
		"\t@SUM(A1)":         "'\t@SUM(A1)",
		"Groceries":          "Groceries",
		"  Rent":             "  Rent",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFiveRealSignupsFitInTheLimit walks the actual two-step form -- first the
// password, then the confirmation page -- with a typo along the way. Only
// accounts actually created count, so five people on one network can all sign
// up, and the sixth account is refused.
func TestFiveRealSignupsFitInTheLimit(t *testing.T) {
	rig := newRig(t)
	step := func(email, confirm string, withConfirm bool) int {
		rig.cookies = nil
		form := url.Values{
			"csrf_token": {rig.csrf("/register")},
			"email":      {email},
			"password":   {"longenough123"},
		}
		if withConfirm {
			form.Set("confirm", confirm)
		}
		return rig.do("POST", "/register", form).Code
	}
	signup := func(i int) int {
		email := fmt.Sprintf("person%d@example.com", i)
		step(email, "", false)           // the confirm step is shown
		step(email, "a typo here", true) // a mistyped confirmation
		return step(email, "longenough123", true)
	}

	for i := 0; i < int(store.SignupLimit.Max); i++ {
		if code := signup(i); code != http.StatusSeeOther {
			t.Fatalf("real sign-up %d of %d refused: status %d", i+1, store.SignupLimit.Max, code)
		}
	}
	if code := signup(99); code != http.StatusTooManyRequests {
		t.Fatalf("sign-up over the limit: status %d, want 429", code)
	}
	if exists, _ := rig.store.EmailExists(context.Background(), "person99@example.com"); exists {
		t.Error("the account over the limit was created")
	}
}

// TestFutureDatedSavingsTransferIsRefused: a transfer cannot be edited or
// deleted, so a future date would stop the fund ever being closed.
func TestFutureDatedSavingsTransferIsRefused(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{
		Kind: store.KindIncome, Label: "Pay", Amount: 10000, OccurredOn: store.Today(),
	}); err != nil {
		t.Fatal(err)
	}
	fund, err := rig.store.CreateFund(ctx, rig.scope, "Car", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, action := range []string{"deposit", "withdraw"} {
		rec := rig.post(fmt.Sprintf("/funds/%d/%s", fund, action),
			url.Values{"amount": {"10.00"}, "date": {"2199-01-01"}, "back": {"/dashboard?tab=current"}})
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s with a future date: status %d", action, rec.Code)
		}
	}
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE fund_id = ?`, fund).Scan(&n)
	if n != 0 {
		t.Fatalf("%d future-dated transfer(s) were stored", n)
	}

	// Today's deposit is fine, and the fund can still be closed.
	if rec := rig.post(fmt.Sprintf("/funds/%d/deposit", fund), url.Values{"amount": {"10.00"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deposit today: %d", rec.Code)
	}
	if rec := rig.post(fmt.Sprintf("/funds/%d/close", fund), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("close: status %d", rec.Code)
	}
}

// TestClosingAFundWithOldFutureTransfersExplainsWhy covers data saved before
// the rule existed: the user gets a reason, not a 500.
func TestClosingAFundWithOldFutureTransfersExplainsWhy(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	fund, err := rig.store.CreateFund(ctx, rig.scope, "Car", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`
		INSERT INTO transactions(household_id, user_id, kind, label, amount_cents, occurred_on, fund_id)
		VALUES (?, ?, 'fund_deposit', 'Car', 1000, '2199-01-01', ?)`,
		rig.scope.HouseholdID, rig.userID, fund); err != nil {
		t.Fatal(err)
	}
	rec := rig.post(fmt.Sprintf("/funds/%d/close", fund), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("close: status %d, want a redirect with a message", rec.Code)
	}
	page := rig.do("GET", rec.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(page, "dated after today") {
		t.Error("the page does not say why the fund could not be closed")
	}
}
