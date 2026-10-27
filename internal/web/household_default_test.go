package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestDefaultSharedBudgetName covers how the default is built from a user's name.
func TestDefaultSharedBudgetName(t *testing.T) {
	cases := []struct {
		user store.User
		want string
	}{
		{store.User{DisplayName: "Kushith"}, "kushith-budget"},
		{store.User{Email: "kushith@example.com"}, "kushith-budget"},
		{store.User{DisplayName: "  Kushith   Reddy "}, "kushith-reddy-budget"},
		{store.User{}, "my-budget"},
	}
	for _, c := range cases {
		if got := defaultSharedBudgetName(c.user); got != c.want {
			t.Errorf("defaultSharedBudgetName(%+v) = %q, want %q", c.user, got, c.want)
		}
	}

	long := defaultSharedBudgetName(store.User{DisplayName: strings.Repeat("é", 80)})
	if len(long) > maxHouseholdName || !strings.HasSuffix(long, sharedBudgetSuffix) {
		t.Errorf("long name not trimmed to fit: %q (%d bytes)", long, len(long))
	}
}

// TestSharingPagePrefillsBudgetName checks the field is filled in, and that a
// blank submission still creates a budget under the default name.
func TestSharingPagePrefillsBudgetName(t *testing.T) {
	rig := newRig(t)
	rig.login()

	// testEmail is tester@example.com, so the display name is "tester".
	body := rig.do("GET", "/household", nil).Body.String()
	if !strings.Contains(body, `value="tester-budget"`) {
		t.Fatalf("the shared budget name is not prefilled with tester-budget")
	}

	if rec := rig.post("/household", url.Values{"name": {"  "}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create with a blank name: %d", rec.Code)
	}
	hh, err := rig.store.ActiveHousehold(context.Background(), store.User{ID: rig.userID})
	if err != nil {
		t.Fatal(err)
	}
	if hh.Personal || hh.Name != "tester-budget" {
		t.Errorf("blank submission created %q (personal=%v), want shared tester-budget", hh.Name, hh.Personal)
	}

	// A typed name still wins over the default.
	if rec := rig.post("/household", url.Values{"name": {"Family"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create with a typed name: %d", rec.Code)
	}
	hh, err = rig.store.ActiveHousehold(context.Background(), store.User{ID: rig.userID})
	if err != nil {
		t.Fatal(err)
	}
	if hh.Name != "Family" {
		t.Errorf("typed name was replaced: got %q, want Family", hh.Name)
	}
}
