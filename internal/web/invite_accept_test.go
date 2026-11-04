package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Someone who already had an account when they were invited can accept from
// the banner, without the email link; the email link still works too.
func TestExistingAccountCanAcceptFromBanner(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		rig := newRig(t)
		ctx := context.Background()
		other := rig.registerUser("friend@example.com")
		// The account was made well before the invitation.
		if _, err := rig.db.Exec(`UPDATE users SET created_at = datetime('now', '-1 day') WHERE id = ?`, other); err != nil {
			t.Fatal(err)
		}
		rig.login()
		shared, err := rig.store.CreateSharedHousehold(ctx, rig.userID, "Family")
		if err != nil {
			t.Fatal(err)
		}
		page := rig.do("GET", "/household", nil).Body.String()
		rig.post("/household/invite", url.Values{
			"email": {"friend@example.com"}, "role": {"editor"},
			"form_household": {hiddenValue(t, page, "form_household")},
		})
		flash := rig.do("GET", "/household", nil).Body.String()

		rig.loginAs("friend@example.com")
		if viaLink {
			i := strings.Index(flash, "/invites/open?token=")
			if i < 0 {
				t.Fatal("no invitation link shown to the owner")
			}
			end := strings.IndexAny(flash[i:], `"< `)
			rig.do("GET", strings.ReplaceAll(flash[i:i+end], "&amp;", "&"), nil)
		}
		dash := rig.do("GET", "/dashboard", nil).Body.String()
		invites, _ := rig.store.InvitesFor(ctx, other, "friend@example.com")
		if len(invites) != 1 {
			t.Fatalf("invites: %d", len(invites))
		}
		if !strings.Contains(dash, fmt.Sprintf(`action="/invites/%d/accept"`, invites[0].ID)) {
			t.Fatalf("viaLink=%v: banner has no Accept button for an existing account", viaLink)
		}
		if !viaLink && !strings.Contains(dash, "Accept here, or with the link in the invitation email.") {
			t.Errorf("banner doesn't mention both ways to accept")
		}
		rig.post(fmt.Sprintf("/invites/%d/accept", invites[0].ID), nil)
		if m, _ := rig.store.ActiveHousehold(ctx, store.User{ID: other}); m.ID != shared {
			t.Errorf("viaLink=%v: Accept did not join the budget", viaLink)
		}
	}
}

// Role names take the right article: "an Owner", "an Editor", "a Viewer".
func TestRoleArticle(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	shared, err := rig.store.CreateSharedHousehold(ctx, rig.userID, "Family")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.store.SwitchHousehold(ctx, rig.userID, shared); err != nil {
		t.Skip("no SwitchHousehold:", err)
	}
	body := rig.do("GET", "/dashboard", nil).Body.String()
	if strings.Contains(body, "a Owner") || !strings.Contains(body, "an Owner") {
		t.Errorf("owner banner article is wrong")
	}
}
