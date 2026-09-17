package web

// Regression tests for the security pass. Each one names the hole it closes, so
// a future change that reopens one fails with an explanation rather than a
// bare assertion.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// ── account enumeration ───────────────────────────────────────────────────────

// Registration answers "does this address have an account here?" out loud, which
// /auth and /forgot both refuse to do. It cannot honestly refuse as well without
// an email round-trip this deployment does not have, so the fact stays available
// and the RATE does not: confirming one suspected address is possible, walking a
// list is not. The same limiter stops an unlimited bcrypt being run from a
// browser tab.
func TestRegisterCannotBeUsedToWalkAListOfAddresses(t *testing.T) {
	rig := newRig(t)

	// One address that exists. Probing it is what an enumerator does, and the
	// 409 it earns is the answer they are paying for.
	if _, err := rig.store.CreateUser(context.Background(), "taken@example.com", "hash"); err != nil {
		t.Fatal(err)
	}

	token := rig.csrf("/register")
	form := url.Values{
		"csrf_token": {token},
		"email":      {"taken@example.com"},
		"password":   {"correct horse battery staple"},
		"confirm":    {"correct horse battery staple"},
	}

	const tries = 40
	var answered, blocked int
	for i := 0; i < tries; i++ {
		switch rig.do("POST", "/register", form).Code {
		case http.StatusConflict:
			answered++
		case http.StatusTooManyRequests:
			blocked++
		}
	}

	if blocked == 0 {
		t.Fatalf("%d probes of a known address, every one answered — nothing is rate limited", tries)
	}
	if answered == 0 {
		t.Fatal("no probe was answered at all; the test is not exercising the path it claims to")
	}
	t.Logf("%d probes answered, %d refused by the limiter", answered, blocked)
}

// ── CSRF on the two public POSTs ──────────────────────────────────────────────

// Both forms rendered a token and neither handler read it. On /forgot that let
// any third-party page spend a visitor's per-IP reset budget, so their own
// genuine request would be refused.
func TestForgotRefusesAPostWithoutItsToken(t *testing.T) {
	rig := newRig(t)

	// Prime the session so a token exists to omit.
	rig.do("GET", "/forgot", nil)

	rec := rig.do("POST", "/forgot", url.Values{"email": {"someone@example.com"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — /forgot accepted a POST with no CSRF token", rec.Code)
	}
}

func TestForgotAcceptsAPostWithItsToken(t *testing.T) {
	rig := newRig(t)

	rec := rig.do("POST", "/forgot", url.Values{
		"csrf_token": {rig.csrf("/forgot")},
		"email":      {"someone@example.com"},
	})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("a legitimate /forgot submission was refused: %s", truncate(rec.Body.String()))
	}
}

func TestResetRefusesAPostWithoutItsToken(t *testing.T) {
	rig := newRig(t)
	rig.do("GET", "/reset?token=whatever", nil)

	rec := rig.do("POST", "/reset", url.Values{
		"token":    {"whatever"},
		"password": {"correct horse battery staple"},
		"confirm":  {"correct horse battery staple"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — /reset accepted a POST with no CSRF token", rec.Code)
	}
}

// ── GETs that write ───────────────────────────────────────────────────────────

// /income and /expense catch recurring schedules up on the way in, which makes
// them GETs that write. An <img src="https://yaba.example/income"> on any page
// an editor visited used to drive that write in their name.
func TestACrossSiteGetDoesNotDriveTheRecurringCatchUp(t *testing.T) {
	for _, page := range []string{"/income", "/expense"} {
		t.Run(page, func(t *testing.T) {
			rig := newRig(t)
			rig.login()

			// A schedule that owes something right now.
			if page == "/income" {
				if _, err := rig.store.CreateRecurringIncome(
					context.Background(), rig.scope, "Salary", 100000, 1, "month", "2026-01-01",
				); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := rig.store.CreateRecurringExpense(
					context.Background(), rig.scope, "Rent", 100000, nil, true, 1, "month", "2026-01-01",
				); err != nil {
					t.Fatal(err)
				}
			}

			req := rig.request("GET", page, nil)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rig.serve(req)

			_, total, err := rig.store.List(context.Background(), rig.scope, store.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if total != 0 {
				t.Fatalf("a cross-site GET of %s generated %d transactions", page, total)
			}

			// The same page reached normally still does the work.
			rig.do("GET", page, nil)
			_, total, err = rig.store.List(context.Background(), rig.scope, store.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if total == 0 {
				t.Fatalf("a same-site GET of %s generated nothing — the catch-up is now broken", page)
			}
		})
	}
}

// ── open redirect ─────────────────────────────────────────────────────────────

// backTo keeps only the path from the Referer, but "//evil.example/x" IS a path
// to url.Parse and a whole different site to a browser.
func TestBackToRefusesAProtocolRelativeReferer(t *testing.T) {
	rig, _, _, viewer := sharedRig(t)
	rig.loginAs(viewer)

	for _, ref := range []string{
		"https://yaba.example//evil.example/x",
		"https://yaba.example/\\evil.example/x",
	} {
		req := rig.request("POST", "/transactions/new", url.Values{
			"csrf_token": {rig.csrf("/dashboard")},
			"kind":       {"expense"},
			"label":      {"Food"},
			"amount":     {"1.00"},
			"date":       {"2026-01-01"},
		})
		req.Header.Set("Referer", ref)
		rec := rig.serve(req)

		if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
			t.Errorf("Referer %q produced Location %q — that leaves the site", ref, loc)
		}
	}
}
