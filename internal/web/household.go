package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Shared budgeting: the settings page and the actions on it.

// householdView backs /household.
type householdView struct {
	view

	Members []store.Member
	Pending []store.Invite

	// Roles are the roles an owner may assign, for the per-member selector.
	Roles []store.Role

	// Activity is the household's recent history: who changed what, and when.
	Activity []store.AuditEntry

	// MailEnabled drives what the page says about delivery.
	MailEnabled bool
}

// recentActivityLimit is how many history entries the sharing page shows.
const recentActivityLimit = 3

// assignableRoles excludes nothing: an owner may promote another member to
// owner, which is how ownership is handed over before somebody leaves.
func assignableRoles() []store.Role {
	return []store.Role{store.RoleOwner, store.RoleEditor, store.RoleViewer}
}

// handleHousehold shows who can see this budget, readable by every member including a
// viewer: somebody who can see the numbers should be able to see who else can.
func (s *Server) handleHousehold(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	v := householdView{
		view:        s.baseView(w, r, "Sharing", "household"),
		Roles:       assignableRoles(),
		MailEnabled: s.mail.Enabled(),
	}

	var err error
	if v.Members, err = s.store.Members(r.Context(), hh.ID, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Pending invitations are the owner's business: a viewer has no action to take on
	// them, and the addresses of people who have not yet accepted are not theirs to read.
	if hh.Role.CanManageMembers() {
		if v.Pending, err = s.store.PendingInvites(r.Context(), hh.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	// The history is for every member, not only the owner: who deleted the rent entry is
	// exactly what an editor needs answered.
	activity, err := s.store.AuditLog(r.Context(), scopeOf(r), 40)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, e := range activity {
		if e.Entity == "invitation" && !hh.Role.CanManageMembers() {
			continue
		}
		v.Activity = append(v.Activity, e)
		if len(v.Activity) == recentActivityLimit {
			break
		}
	}

	s.render(w, r, "household.html", v)
}

// handleHouseholdCreate makes a new shared budget and switches to it.
func (s *Server) handleHouseholdCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if !s.parseForm(w, r) {
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if _, err := s.store.CreateSharedHousehold(r.Context(), user.ID, name); err != nil {
		s.redirectError(w, r, "/household",
			s.safeMessage(r, err, "That budget could not be created. Try again."))
		return
	}

	s.redirectSuccess(w, r, "/household", fmt.Sprintf("Created %q. Invite someone to join it.", name))
}

// handleHouseholdSwitch changes which budget the user is looking at.
func (s *Server) handleHouseholdSwitch(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	if !s.parseForm(w, r) {
		return
	}

	id, err := strconv.ParseInt(r.PostFormValue("household_id"), 10, 64)
	if err != nil {
		s.badRequest(w, "That is not a budget id.")
		return
	}

	switch err := s.store.SwitchHousehold(r.Context(), user.ID, id); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "You do not have access to that budget.")
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	// Back to the dashboard rather than to the referring page: the whole point
	// of switching is to look at different numbers, and every figure on the
	// previous page belonged to the household just left.
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleHouseholdRename renames the current shared budget.
func (s *Server) handleHouseholdRename(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	if !s.parseForm(w, r) {
		return
	}

	if err := s.store.RenameHousehold(r.Context(), hh.ID, r.PostFormValue("name")); err != nil {
		s.flashError(w, r, s.safeMessage(r, err, "That change could not be saved. Try again."))
	} else {
		s.flashSuccess(w, r, "Name updated.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleHouseholdDelete removes a shared budget and everything in it.
func (s *Server) handleHouseholdDelete(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	orphaned, err := s.store.DeleteHousehold(r.Context(), hh.ID)
	s.removeReceiptFiles(orphaned)
	switch {
	case errors.Is(err, store.ErrPersonalHousehold):
		s.redirectError(w, r, "/household", "This is your own budget, so it cannot be deleted.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, "/dashboard", fmt.Sprintf("Deleted %q. You are back in your own budget.", hh.Name))
}

// handleHouseholdLeave is a member removing themselves from a shared budget.
func (s *Server) handleHouseholdLeave(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	switch err := s.store.LeaveHousehold(r.Context(), hh.ID, user.ID); {
	case errors.Is(err, store.ErrPersonalHousehold):
		s.redirectError(w, r, "/household", "This is your own budget, so there is nothing to leave.")
		return
	case errors.Is(err, store.ErrLastOwner):
		// Deliberately specific. "You cannot leave" would be baffling; the user
		// needs to know the fix is to promote somebody first.
		s.flashError(w, r,
			"You are the only owner of this budget. Make someone else an owner first, "+
				"or delete the budget instead.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, "/dashboard", fmt.Sprintf("You have left %q.", hh.Name))
}

// ── invitations ───────────────────────────────────────────────────────────────

// handleInviteCreate invites an email address to the current budget.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if !s.parseForm(w, r) {
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	role := store.Role(r.PostFormValue("role"))

	// The same address validation the login form uses, so "kushith" is rejected
	// here for the same reason and with the same wording it is rejected there.
	if msg := validateEmail(email); msg != "" {
		s.redirectError(w, r, "/household", msg)
		return
	}

	// Inviting a personal budget makes no sense: it is one person's private space.
	if hh.Personal {
		s.flashError(w, r,
			"This is your own private budget. Create a shared budget below, then invite people to that.")
		http.Redirect(w, r, "/household", http.StatusSeeOther)
		return
	}

	if store.NormalizeEmail(email) == store.NormalizeEmail(user.Email) {
		s.redirectError(w, r, "/household", "You are already in this budget.")
		return
	}

	if !s.inviteAllowed(w, r, user.ID, email) {
		return
	}

	switch err := s.store.InviteMember(r.Context(), hh.ID, user.ID, email, role); {
	case errors.Is(err, store.ErrAlreadyMember):
		s.flashError(w, r, "That person is already in this budget.")
	case errors.Is(err, store.ErrInviteOpen):
		s.flashError(w, r, "They already have an invitation waiting.")
	case err != nil:
		s.flashError(w, r, s.safeMessage(r, err, "That change could not be saved. Try again."))
	default:
		s.countInvite(r, user.ID, email)
		s.sendInvitation(w, r, store.NormalizeEmail(email), hh.Name, user.DisplayName, role)
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// inviteKeys are the two counters an invitation is charged to: the account
// sending it and the address receiving it.
func inviteKeys(userID int64, email string) (sender, recipient string) {
	return "invite|from|" + strconv.FormatInt(userID, 10),
		"invite|to|" + store.NormalizeEmail(email)
}

// inviteAllowed checks the invitation limits before anything is created or
// emailed. Every invitation is an email sent through this server's relay to an
// address the sender chose, so without a limit an open sign-up page was an
// open spam relay. On refusal it flashes the reason, redirects, and returns
// false.
func (s *Server) inviteAllowed(w http.ResponseWriter, r *http.Request, userID int64, email string) bool {
	sender, recipient := inviteKeys(userID, email)
	wait, err := s.store.RateRetryInFor(r.Context(), sender, store.InviteLimit)
	if err == nil && wait == 0 {
		wait, err = s.store.RateRetryInFor(r.Context(), recipient, store.InviteRecipientLimit)
	}
	if err != nil {
		s.serverError(w, r, err)
		return false
	}
	if wait > 0 {
		log.Printf("invite refused: user=%d over the invitation limit", userID)
		s.redirectError(w, r, "/household",
			"Too many invitations have been sent. Try again "+retryPhrase(wait)+".")
		return false
	}
	return true
}

// countInvite charges one invitation (new or resent) to both counters.
func (s *Server) countInvite(r *http.Request, userID int64, email string) {
	sender, recipient := inviteKeys(userID, email)
	for _, c := range []struct {
		key   string
		limit store.RateLimit
	}{{sender, store.InviteLimit}, {recipient, store.InviteRecipientLimit}} {
		if err := s.store.RateHit(r.Context(), c.key, c.limit); err != nil {
			log.Printf("WARN  %s could not count an invitation: %v", requestID(r.Context()), err)
		}
	}
}

// handleInviteRevoke withdraws an invitation this household sent.
func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	hh := mustMembership(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	// The household id is part of the WHERE clause in RevokeInvite, so an owner
	// cannot cancel another household's invitation by guessing its id.
	switch err := s.store.RevokeInvite(r.Context(), id, hh.ID); {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That invitation is no longer waiting.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, "Invitation withdrawn.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleInviteAccept joins the household an invitation names.
func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	switch err := s.store.AcceptInvite(r.Context(), id, user.ID, user.Email); {
	case errors.Is(err, store.ErrInviteExpired):
		// Distinguished from "no longer available" on purpose.
		s.flashError(w, r,
			"That invitation has expired. Ask them to send it again — invitations last 24 hours.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrNotFound):
		// Same message whether the invitation never existed, was withdrawn, or
		// belongs to somebody else -- a probing user learns nothing either way.
		s.redirectError(w, r, "/dashboard", "That invitation is no longer available.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, "/dashboard", "You have joined. This is the shared budget.")
}

// handleInviteDecline refuses an invitation.
func (s *Server) handleInviteDecline(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	if err := s.store.DeclineInvite(r.Context(), id, user.Email); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(w, r, backTo(r), "Invitation declined.")
}

// ── members ───────────────────────────────────────────────────────────────────

// handleMemberRole changes one member's role.
func (s *Server) handleMemberRole(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	if !s.parseForm(w, r) {
		return
	}

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
		return
	}

	role := store.Role(r.PostFormValue("role"))
	if !role.Valid() {
		s.redirectError(w, r, "/household", "Choose Owner, Editor or Viewer.")
		return
	}

	switch err := s.store.SetRole(r.Context(), hh.ID, user.ID, target, role); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrLastOwner):
		s.flashError(w, r,
			"This budget needs at least one owner. Make someone else an owner first.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		if target == user.ID {
			// Demoting yourself takes effect on the next request, so say so --
			// otherwise the page reloads with fewer buttons and no explanation.
			s.flashSuccess(w, r, "Your own role is now "+strings.ToLower(role.Label())+".")
		} else {
			s.flashSuccess(w, r, "Role updated.")
		}
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleMemberRemove takes somebody out of the household.
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
		return
	}

	switch err := s.store.RemoveMember(r.Context(), hh.ID, user.ID, target); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrLastOwner):
		s.flashError(w, r,
			"This budget needs at least one owner. Make someone else an owner first.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, "Removed. Anything they entered stays in the budget.")
		if target == user.ID {
			// An owner who removed themselves is no longer looking at this
			// household; RemoveMember has already moved them home.
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// ── invitation delivery ───────────────────────────────────────────────────────

// sendInvitation emails the invitation and reports honestly what happened.
func (s *Server) sendInvitation(w http.ResponseWriter, r *http.Request,
	email, household, invitedBy string, role store.Role) {
	roleWord := strings.ToLower(role.Label())

	if !s.mail.Enabled() {
		s.flashSuccess(w, r, fmt.Sprintf(
			"%s is invited as a %s, and the invitation expires in 24 hours. "+
				"Email is not configured, so let them know — they will see it when they sign in.",
			email, roleWord))
		return
	}

	err := s.mail.Invitation(r.Context(), email, household, invitedBy,
		string(role), store.InviteTTL)
	if err != nil {
		// The invitation row exists and is perfectly usable, so this is not a failure of the
		// invitation -- only of its delivery.
		// The address is deliberately not logged. Every other line in this file
		// identifies people by a numeric id; a plain-text log that is shipped and
		// retained is not the place for somebody's email address, and the
		// household name is enough to find the invitation row.
		log.Printf("WARN  %s invitation email failed for budget %q: %v",
			requestID(r.Context()), household, err)
		s.flashError(w, r, fmt.Sprintf(
			"%s is invited as a %s, but the email could not be sent. "+
				"Tell them to sign in within 24 hours, or resend it below.",
			email, roleWord))
		return
	}

	s.flashSuccess(w, r, fmt.Sprintf(
		"Invitation emailed to %s as a %s. It expires in 24 hours.", email, roleWord))
}

// handleInviteResend gives an unanswered invitation another 24 hours and emails
// it again.
func (s *Server) handleInviteResend(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	id, ok := s.pathIDOrBadRequest(w, r, "an invitation id")
	if !ok {
		return
	}

	// Checked against the invitation's own address, so it has to be read
	// first; the household id scopes the read, as it does the resend.
	pending, err := s.store.PendingInvites(r.Context(), hh.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, p := range pending {
		if p.ID == id && !s.inviteAllowed(w, r, user.ID, p.Email) {
			return
		}
	}

	// The household id is passed in, so an owner of one budget cannot refresh an
	// invitation belonging to another.
	inv, err := s.store.ResendInvite(r.Context(), hh.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.redirectError(w, r, "/household", "That invitation is no longer waiting for an answer.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.countInvite(r, user.ID, inv.Email)
	s.sendInvitation(w, r, inv.Email, inv.HouseholdName, user.DisplayName, inv.Role)
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}

// handleTransferOwnership hands the budget to another member.
func (s *Server) handleTransferOwnership(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	hh := mustMembership(r)

	target, ok := s.pathIDOrBadRequest(w, r, "a member id")
	if !ok {
		return
	}

	switch err := s.store.TransferOwnership(r.Context(), hh.ID, user.ID, target); {
	case errors.Is(err, store.ErrNotMember):
		s.flashError(w, r, "That person is not in this budget.")
	case errors.Is(err, store.ErrForbidden):
		s.flashError(w, r, "Only the owner can hand over a budget.")
	case err != nil:
		s.flashError(w, r, s.safeMessage(r, err, "That change could not be saved. Try again."))
	default:
		s.flashSuccess(w, r,
			"Ownership handed over. You are now an editor of this budget, "+
				"so you can still add and change entries but not move savings.")
	}
	http.Redirect(w, r, "/household", http.StatusSeeOther)
}
