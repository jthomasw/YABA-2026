package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// A household is the thing that owns money, and a Role is what a person may do to it.

// ── roles ─────────────────────────────────────────────────────────────────────

// Role is a member's authority within one household.
type Role string

const (
	// RoleOwner administers the household: members, invitations, renaming,
	// deletion, and moving money between savings funds.
	RoleOwner Role = "owner"

	// RoleEditor records the household's day-to-day money -- income, expenses,
	// recurring bills, category budgets -- but does not administer it and does
	// not move money into or out of savings funds.
	RoleEditor Role = "editor"

	// RoleViewer reads everything and changes nothing.
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a role this application recognises.
func (r Role) Valid() bool {
	return r == RoleOwner || r == RoleEditor || r == RoleViewer
}

// Label is the human name for a role.
func (r Role) Label() string {
	switch r {
	case RoleOwner:
		return "Owner"
	case RoleEditor:
		return "Editor"
	case RoleViewer:
		return "Viewer"
	}
	return "No access"
}

// Explain is the one-line description shown beside the role in the UI.
func (r Role) Explain() string {
	switch r {
	case RoleOwner:
		return "Full control, including members and savings funds."
	case RoleEditor:
		return "Can add and edit income and expenses, but not move savings."
	case RoleViewer:
		return "Can see everything. Cannot change anything."
	}
	return ""
}

// CanEditEntries covers income, expenses, recurring expense buckets, category
// budgets and reallocation -- everything that records what the household
// actually earned and spent.
func (r Role) CanEditEntries() bool { return r == RoleOwner || r == RoleEditor }

// CanMoveFunds covers depositing to, withdrawing from and closing a savings fund, and
// is owner-only.
func (r Role) CanMoveFunds() bool { return r == RoleOwner }

// CanManageMembers covers inviting, removing, and changing roles.
func (r Role) CanManageMembers() bool { return r == RoleOwner }

// CanManageHousehold covers renaming and deleting the household itself.
func (r Role) CanManageHousehold() bool { return r == RoleOwner }

// ── errors ────────────────────────────────────────────────────────────────────

var (
	// ErrNotMember means the caller is not in the household they asked about.
	ErrNotMember = errors.New("not a member of that household")

	// ErrForbidden means the caller is a member but their role does not permit the action.
	ErrForbidden = errors.New("your role does not allow that")

	// ErrLastOwner blocks removing or demoting the only owner, which would
	// leave a household nobody could administer.
	ErrLastOwner = errors.New("a household must always have at least one owner")

	// ErrAlreadyMember is returned instead of a UNIQUE violation when inviting
	// somebody who has already joined.
	ErrAlreadyMember = errors.New("that person is already a member")

	// ErrInviteOpen means an unanswered invitation for that address exists.
	ErrInviteOpen = errors.New("that address already has an invitation pending")

	// ErrPersonalHousehold blocks administering a personal household as though
	// it were shared: it cannot be renamed away, left, or deleted, because it is
	// where the user's own data lives.
	ErrPersonalHousehold = errors.New("your personal budget cannot be shared or removed")
)

// ── types ─────────────────────────────────────────────────────────────────────

// Household is one budget that one or more people work on.
type Household struct {
	ID       int64
	Name     string
	Personal bool
	Members  int
}

// Membership is a household together with the caller's authority in it.
type Membership struct {
	Household
	Role Role
}

// Member is one person in a household, as shown on the settings page.
type Member struct {
	UserID      int64
	Email       string
	DisplayName string
	Role        Role
	JoinedAt    string
	IsSelf      bool
}

// Name is what to show for a member, preferring the display name.
func (m Member) Name() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	if i := strings.IndexByte(m.Email, '@'); i > 0 {
		return m.Email[:i]
	}
	return m.Email
}

// Invite is an unanswered invitation.
type Invite struct {
	ID            int64
	HouseholdID   int64
	HouseholdName string
	Email         string
	Role          Role
	InvitedBy     string
	CreatedAt     string

	// ExpiresAt is when the invitation stops being acceptable, and Expired says whether
	// that has happened.
	ExpiresAt string
	Expired   bool

	// TokenHash is the hash of the secret emailed with the invitation, or ""
	// for one sent before invitations had one. Accepting needs that secret
	// (see AcceptInvite); it is never shown.
	TokenHash string

	// AccountPredates is true when the invited person's account already
	// existed when the invitation was sent. The owner then invited a known
	// account rather than an address somebody might register afterwards, so
	// the in-app Accept button is enough (see AcceptInvite).
	AccountPredates bool

	// NeedsLink is set by the web layer for the invitation banner: true when
	// accepting needs the invitation's email link -- the account was created
	// after the invitation and this browser hasn't opened the link yet.
	NeedsLink bool
}

// ErrInviteNeedsLink refuses to accept an invitation without the secret from
// its email: owning an account with the invited address is not proof of owning
// the mailbox, since nothing verifies an address at sign-up.
var ErrInviteNeedsLink = errors.New("open the link in the invitation email to accept")

// ── creating households ───────────────────────────────────────────────────────

// createPersonalHousehold inserts a user's own household and makes them its owner,
// inside the caller's transaction: a user committed without one could log in and have
// nowhere to put anything. Called by CreateUser, and by ActiveHousehold as a repair.
func createPersonalHousehold(ctx context.Context, tx *sql.Tx, userID int64, display string) (int64, error) {
	name := strings.TrimSpace(display)
	if name == "" {
		name = "My"
	}
	name += "'s budget"

	res, err := tx.ExecContext(ctx,
		`INSERT INTO households(name, personal_for) VALUES(?, ?)`, name, userID)
	if err != nil {
		return 0, fmt.Errorf("create personal household: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`,
		id, userID); err != nil {
		return 0, fmt.Errorf("create owner membership: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET active_household_id = ? WHERE id = ?`, id, userID); err != nil {
		return 0, fmt.Errorf("set active household: %w", err)
	}
	return id, nil
}

// CreateSharedHousehold makes a new shared budget with the caller as owner and
// switches them into it.
func (s *Store) CreateSharedHousehold(ctx context.Context, userID int64, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("give the household a name")
	}
	if len(name) > 60 {
		return 0, fmt.Errorf("that name is too long")
	}

	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// personal_for stays NULL: this is a shared household, and the partial
		// unique index only constrains personal ones.
		res, err := tx.ExecContext(ctx, `INSERT INTO households(name) VALUES(?)`, name)
		if err != nil {
			return fmt.Errorf("create household: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO household_members(household_id, user_id, role) VALUES(?, ?, 'owner')`,
			id, userID); err != nil {
			return fmt.Errorf("add owner: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET active_household_id = ? WHERE id = ?`, id, userID)
		return err
	})
	return id, err
}

// ── resolving the caller's household ──────────────────────────────────────────

// ActiveHousehold returns the household the user is working in and their role in it.
func (s *Store) ActiveHousehold(ctx context.Context, u User) (Membership, error) {
	m, err := s.activeHouseholdOnce(ctx, u.ID)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Membership{}, err
	}

	// Fall back to the personal household, creating it if it is missing.
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM households WHERE personal_for = ?`, u.ID)
		switch scanErr := row.Scan(&id); {
		case errors.Is(scanErr, sql.ErrNoRows):
			var mkErr error
			if id, mkErr = createPersonalHousehold(ctx, tx, u.ID, u.DisplayName); mkErr != nil {
				return mkErr
			}
		case scanErr != nil:
			return scanErr
		default:
			// It exists; make sure the membership row does too before pointing
			// the user at it, or the next request would fall through here again.
			if _, execErr := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO household_members(household_id, user_id, role)
				 VALUES(?, ?, 'owner')`, id, u.ID); execErr != nil {
				return execErr
			}
			if _, execErr := tx.ExecContext(ctx,
				`UPDATE users SET active_household_id = ? WHERE id = ?`, id, u.ID); execErr != nil {
				return execErr
			}
		}
		return nil
	})
	if err != nil {
		return Membership{}, fmt.Errorf("resolve household: %w", err)
	}
	return s.activeHouseholdOnce(ctx, u.ID)
}

// activeHouseholdOnce reads the active household, returning ErrNotFound if the pointer
// is null or no longer backed by a membership.
func (s *Store) activeHouseholdOnce(ctx context.Context, userID int64) (Membership, error) {
	var m Membership
	var personal sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT h.id, h.name, h.personal_for, hm.role,
		       (SELECT COUNT(*) FROM household_members x WHERE x.household_id = h.id)
		FROM users u
		JOIN households         h  ON h.id = u.active_household_id
		JOIN household_members  hm ON hm.household_id = h.id AND hm.user_id = u.id
		WHERE u.id = ?`, userID,
	).Scan(&m.ID, &m.Name, &personal, &m.Role, &m.Members)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, fmt.Errorf("select active household: %w", err)
	}
	m.Personal = personal.Valid
	return m, nil
}

// HouseholdsFor lists every household the user belongs to, for the switcher.
func (s *Store) HouseholdsFor(ctx context.Context, userID int64) ([]Household, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.name, h.personal_for,
		       (SELECT COUNT(*) FROM household_members x WHERE x.household_id = h.id)
		FROM household_members hm
		JOIN households h ON h.id = hm.household_id
		WHERE hm.user_id = ?
		ORDER BY (h.personal_for IS NULL), h.name COLLATE NOCASE, h.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list households: %w", err)
	}
	defer rows.Close()

	var out []Household
	for rows.Next() {
		var h Household
		var personal sql.NullInt64
		if err := rows.Scan(&h.ID, &h.Name, &personal, &h.Members); err != nil {
			return nil, err
		}
		h.Personal = personal.Valid
		out = append(out, h)
	}
	return out, rows.Err()
}

// SwitchHousehold points the user at a different household.
func (s *Store) SwitchHousehold(ctx context.Context, userID, householdID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET active_household_id = ?
		WHERE id = ?
		  AND EXISTS (SELECT 1 FROM household_members
		              WHERE household_id = ? AND user_id = ?)`,
		householdID, userID, householdID, userID)
	if err != nil {
		return fmt.Errorf("switch household: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotMember
	}
	return nil
}

// ── administering a household ─────────────────────────────────────────────────

// RenameHousehold changes the display name of a shared household.
func (s *Store) RenameHousehold(ctx context.Context, householdID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("give the household a name")
	}
	if len(name) > 60 {
		return fmt.Errorf("that name is too long")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE households SET name = ? WHERE id = ?`, name, householdID)
	if err != nil {
		return fmt.Errorf("rename household: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Members lists a household's people, owners first.
func (s *Store) Members(ctx context.Context, householdID, selfID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, ''),
		       hm.role, hm.joined_at
		FROM household_members hm
		JOIN users u ON u.id = hm.user_id
		WHERE hm.household_id = ?
		ORDER BY CASE hm.role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 ELSE 2 END,
		         u.display_name COLLATE NOCASE, u.id`, householdID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		m.IsSelf = m.UserID == selfID
		out = append(out, m)
	}
	return out, rows.Err()
}

// TransferOwnership makes another member an owner and steps the caller down to
// editor, in one transaction.
func (s *Store) TransferOwnership(ctx context.Context, householdID, fromUserID, toUserID int64) error {
	if fromUserID == toUserID {
		return fmt.Errorf("you already own this budget")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// The caller must actually be an owner. Checked here rather than relying
		// on the route's permission wrapper alone, because this is the one action
		// that gives away the ability to perform it.
		var mine Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, fromUserID).Scan(&mine)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if mine != RoleOwner {
			return ErrForbidden
		}

		var theirs Role
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, toUserID).Scan(&theirs)
		if errors.Is(err, sql.ErrNoRows) {
			// Only an existing member can be promoted. Handing a budget to an address that has
			// not accepted an invitation would leave it owned by nobody who can sign in.
			return ErrNotMember
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = 'owner'
			 WHERE household_id = ? AND user_id = ?`, householdID, toUserID); err != nil {
			return fmt.Errorf("promote new owner: %w", err)
		}
		// Demoted to editor rather than removed: the previous owner almost
		// certainly still uses the budget, and quietly ejecting them would be a
		// surprising way for a transfer to end.
		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = 'editor'
			 WHERE household_id = ? AND user_id = ?`, householdID, fromUserID); err != nil {
			return fmt.Errorf("step down: %w", err)
		}

		// Belt and braces: prove the invariant this method exists to protect --
		// the budget still has an owner -- before committing. It used to demand
		// exactly one, which refused every transfer in a budget that already
		// had a co-owner and showed the user "transfer would leave 2 owners".
		owners, err := ownerCount(ctx, tx, householdID)
		if err != nil {
			return err
		}
		if owners < 1 {
			return ErrLastOwner
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: fromUserID},
			"transferred ownership", "member", toUserID, "and stepped down to editor")
	})
}

// SetRole changes one member's role. Demoting the last owner is refused, and the count
// and the update share a transaction, so two owners cannot simultaneously demote each
// other and leave the household with none.
func (s *Store) SetRole(ctx context.Context, householdID, actorID, targetID int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("unknown role")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var current Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if current == role {
			return nil
		}

		if current == RoleOwner {
			if err := requireAnotherOwner(ctx, tx, householdID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE household_members SET role = ? WHERE household_id = ? AND user_id = ?`,
			role, householdID, targetID); err != nil {
			return err
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: actorID},
			"changed a role", "member", targetID, fmt.Sprintf("to %s", role))
	})
}

func ownerCount(ctx context.Context, tx *sql.Tx, householdID int64) (int, error) {
	var owners int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM household_members WHERE household_id = ? AND role = 'owner'`,
		householdID).Scan(&owners)
	return owners, err
}

// requireAnotherOwner refuses to demote or remove the only owner, which would
// leave a household nobody could administer. Called inside the same transaction
// as the change, so two owners cannot simultaneously demote each other.
func requireAnotherOwner(ctx context.Context, tx *sql.Tx, householdID int64) error {
	owners, err := ownerCount(ctx, tx, householdID)
	if err != nil {
		return err
	}
	if owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// RemoveMember takes somebody out of a household.
func (s *Store) RemoveMember(ctx context.Context, householdID, actorID, targetID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var role Role
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}

		if role == RoleOwner {
			if err := requireAnotherOwner(ctx, tx, householdID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM household_members WHERE household_id = ? AND user_id = ?`,
			householdID, targetID); err != nil {
			return err
		}

		// Anyone left pointing at this household is moved back to their own, or their pointer
		// is cleared for ActiveHousehold to repair.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET active_household_id = (SELECT id FROM households WHERE personal_for = users.id)
			WHERE id = ? AND active_household_id = ?`, targetID, householdID); err != nil {
			return err
		}
		// Their entries stay; only the membership goes.
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: actorID},
			"removed a member", "member", targetID, "their entries were kept")
	})
}

// DeleteHousehold removes a shared household and everything in it, returning
// the receipt files that belonged to it for the caller to delete from disk.
func (s *Store) DeleteHousehold(ctx context.Context, householdID int64) (orphaned []string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var personal sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT personal_for FROM households WHERE id = ?`, householdID).Scan(&personal)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if personal.Valid {
			return ErrPersonalHousehold
		}

		// Move everyone out first. users.active_household_id is ON DELETE SET
		// NULL, so this is belt and braces -- but it means members land back in
		// their own budget rather than on a page that has to repair itself.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET active_household_id = (SELECT id FROM households WHERE personal_for = users.id)
			WHERE active_household_id = ?`, householdID); err != nil {
			return err
		}

		// Every receipt file the household holds, read before the cascade
		// removes the rows that name them.
		paths, err := queryPaths(ctx, tx, `
			SELECT receipt_path FROM transactions WHERE household_id = ? AND receipt_path IS NOT NULL
			UNION
			SELECT path FROM receipt_jobs WHERE household_id = ?`, householdID, householdID)
		if err != nil {
			return fmt.Errorf("list receipt files: %w", err)
		}

		// The household's transactions, funds, buckets, allocations, budgets and
		// receipt jobs all cascade from this one DELETE.
		if _, err = tx.ExecContext(ctx, `DELETE FROM households WHERE id = ?`, householdID); err != nil {
			return err
		}
		orphaned, err = unreferenced(ctx, tx, paths)
		return err
	})
	if err != nil {
		return nil, err
	}
	return orphaned, nil
}

// LeaveHousehold is a member removing themselves.
func (s *Store) LeaveHousehold(ctx context.Context, householdID, userID int64) error {
	var personal sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT personal_for FROM households WHERE id = ?`, householdID).Scan(&personal)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if personal.Valid {
		return ErrPersonalHousehold
	}
	// Leaving is removing yourself, so the actor and the target are the same
	// person — which is exactly what the history should say.
	return s.RemoveMember(ctx, householdID, userID, userID)
}
