package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── invitations ───────────────────────────────────────────────────────────────

// InviteMember records an invitation for an email address.
const InviteTTL = 24 * time.Hour

var inviteTTLModifier = after(InviteTTL)

// ErrInviteExpired is returned when an invitation is real but too old to use.
var ErrInviteExpired = errors.New("invitation expired")

// InviteMember creates an invitation and returns its secret, which belongs in
// the link emailed to the invited address; only its hash is stored.
func (s *Store) InviteMember(ctx context.Context, householdID, invitedBy int64, email string, role Role) (string, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return "", fmt.Errorf("enter an email address")
	}
	if role != RoleEditor && role != RoleViewer {
		return "", fmt.Errorf("invite someone as an editor or a viewer")
	}
	token, err := newSessionID()
	if err != nil {
		return "", err
	}

	return token, s.inTx(ctx, func(tx *sql.Tx) error {
		// Already in? Say so plainly rather than creating an invitation that
		// could never be accepted.
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM household_members hm
			JOIN users u ON u.id = hm.user_id
			WHERE hm.household_id = ?
			  AND (u.email = ? COLLATE NOCASE OR u.username = ? COLLATE NOCASE)`,
			householdID, email, email).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyMember
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO household_invites(household_id, email, role, invited_by, expires_at, token_hash)
			VALUES(?, ?, ?, ?, datetime('now', ?), ?)`,
			householdID, email, role, invitedBy, inviteTTLModifier, TokenHash(token))
		if err != nil {
			// idx_invites_open is partial on status='pending', so this can only
			// mean an unanswered invitation already exists.
			if isUniqueViolation(err) {
				return ErrInviteOpen
			}
			return fmt.Errorf("create invite: %w", err)
		}
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: invitedBy},
			"invited", "invitation", 0, fmt.Sprintf("%s as %s", email, role))
	})
}

// PendingInvites lists a household's unanswered invitations, for its settings page.
func (s *Store) PendingInvites(ctx context.Context, householdID int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.household_id, i.email, i.role, i.created_at,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, '')),
		       IFNULL(i.expires_at, ''),
		       i.expires_at IS NOT NULL AND i.expires_at <= datetime('now')
		FROM household_invites i
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.household_id = ? AND i.status = 'pending'
		ORDER BY i.created_at DESC, i.id DESC`, householdID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.HouseholdID, &i.Email, &i.Role,
			&i.CreatedAt, &i.InvitedBy, &i.ExpiresAt, &i.Expired); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ResendInvite gives an unanswered invitation another 24 hours.
// ResendInvite gives an invitation another InviteTTL and a fresh secret,
// returned for the new email; the link in the old one stops working.
func (s *Store) ResendInvite(ctx context.Context, householdID, inviteID int64) (Invite, string, error) {
	var inv Invite
	token, err := newSessionID()
	if err != nil {
		return inv, "", err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE household_invites
			SET expires_at = datetime('now', ?), token_hash = ?
			WHERE id = ? AND household_id = ? AND status = 'pending'`,
			inviteTTLModifier, TokenHash(token), inviteID, householdID)
		if err != nil {
			return fmt.Errorf("extend invite: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return tx.QueryRowContext(ctx, `
			SELECT i.id, i.household_id, h.name, i.email, i.role, IFNULL(i.expires_at, '')
			FROM household_invites i
			JOIN households h ON h.id = i.household_id
			WHERE i.id = ?`, inviteID,
		).Scan(&inv.ID, &inv.HouseholdID, &inv.HouseholdName, &inv.Email,
			&inv.Role, &inv.ExpiresAt)
	})
	return inv, token, err
}

// InviteByToken finds the unanswered, unexpired invitation an emailed secret
// belongs to.
func (s *Store) InviteByToken(ctx context.Context, token string) (Invite, error) {
	var inv Invite
	if strings.TrimSpace(token) == "" {
		return inv, ErrNotFound
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id, i.household_id, h.name, i.email, i.role, IFNULL(i.expires_at, ''), i.token_hash
		FROM household_invites i
		JOIN households h ON h.id = i.household_id
		WHERE i.token_hash = ? AND i.status = 'pending' AND i.expires_at > datetime('now')`,
		TokenHash(token)).Scan(&inv.ID, &inv.HouseholdID, &inv.HouseholdName, &inv.Email,
		&inv.Role, &inv.ExpiresAt, &inv.TokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return inv, ErrNotFound
	}
	if err != nil {
		return inv, fmt.Errorf("find invitation: %w", err)
	}
	return inv, nil
}

// TestOnlyExpireInvite ages an invitation past its window. It is for tests
// only: nothing in production may call it. It is exported, rather than living
// in an export_test.go file, because the web package's tests need it and a
// _test.go file is not visible outside its own package. It is not scoped to a
// household for the same reason -- it has no caller with a session.
func (s *Store) TestOnlyExpireInvite(ctx context.Context, inviteID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE household_invites SET expires_at = datetime('now', '-1 minute') WHERE id = ?`,
		inviteID)
	return err
}

// PurgeStaleInvites removes invitations that expired long enough ago that nobody will
// act on them.
func (s *Store) PurgeStaleInvites(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM household_invites
		WHERE status = 'pending'
		  AND expires_at IS NOT NULL
		  AND expires_at <= datetime('now', '-30 days')`)
	if err != nil {
		return 0, fmt.Errorf("purge stale invites: %w", err)
	}
	return res.RowsAffected()
}

// InvitesFor lists unanswered invitations addressed to one person, matched case-
// insensitively.
func (s *Store) InvitesFor(ctx context.Context, userID int64, email string) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.household_id, h.name, i.role, i.created_at,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, '')),
		       IFNULL(i.expires_at, ''), IFNULL(i.token_hash, ''),
		       i.created_at > IFNULL((SELECT me.created_at FROM users me WHERE me.id = ?), i.created_at)
		FROM household_invites i
		JOIN households h ON h.id = i.household_id
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.status = 'pending'
		  AND i.email = ? COLLATE NOCASE
		  -- A NULL expiry compares as NULL, which is not true, so it is treated
		  -- as expired. Failing closed is the right direction for something that
		  -- grants access to a budget: the worst case is an owner resending.
		  AND i.expires_at > datetime('now')
		  AND NOT EXISTS (SELECT 1 FROM household_members m
		                  WHERE m.household_id = i.household_id AND m.user_id = ?)
		ORDER BY i.created_at ASC, i.id ASC`, userID, NormalizeEmail(email), userID)
	if err != nil {
		return nil, fmt.Errorf("list my invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.HouseholdID, &i.HouseholdName, &i.Role,
			&i.CreatedAt, &i.InvitedBy, &i.ExpiresAt, &i.TokenHash, &i.AccountPredates); err != nil {
			return nil, err
		}
		i.Email = NormalizeEmail(email)
		out = append(out, i)
	}
	return out, rows.Err()
}

// AcceptInvite turns an invitation into a membership and switches the user into the
// household.
//
// proof is the secret from the invitation's email link. It is required for
// every invitation that has one: an account with the invited address is not
// proof of owning that mailbox, so without it whoever registered the address
// first -- or a typo of it -- could join the budget.
func (s *Store) AcceptInvite(ctx context.Context, inviteID, userID int64, email, proof string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var householdID int64
		var role Role
		var expired bool
		var tokenHash string
		var accountPredates bool
		err := tx.QueryRowContext(ctx, `
			SELECT household_id, role,
			       expires_at IS NULL OR expires_at <= datetime('now'),
			       IFNULL(token_hash, ''),
			       created_at > IFNULL((SELECT created_at FROM users WHERE id = ?), created_at)
			FROM household_invites
			WHERE id = ? AND status = 'pending' AND email = ? COLLATE NOCASE`,
			userID, inviteID, NormalizeEmail(email)).Scan(&householdID, &role, &expired, &tokenHash, &accountPredates)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Checked inside the transaction, not before it.
		if expired {
			return ErrInviteExpired
		}
		// The email link is needed only when the account was created after
		// the invitation: that is the case where somebody could have signed
		// up with an address they don't own in order to take it. An account
		// the owner invited because it already existed can accept in the app.
		if tokenHash != "" && tokenHash != TokenHash(proof) && !accountPredates {
			return ErrInviteNeedsLink
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO household_members(household_id, user_id, role)
			VALUES(?, ?, ?)`, householdID, userID, role); err != nil {
			return fmt.Errorf("join household: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE household_invites
			SET status = 'accepted', responded_at = datetime('now')
			WHERE id = ?`, inviteID); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET active_household_id = ? WHERE id = ?`, householdID, userID); err != nil {
			return err
		}
		// The actor is the person joining, which is why this is recorded here
		// rather than where the invitation was sent: accepting is their act.
		return recordAudit(ctx, tx, Scope{HouseholdID: householdID, UserID: userID},
			"joined", "member", userID, fmt.Sprintf("as %s", role))
	})
}

// DeclineInvite marks an invitation refused. The row is kept rather than deleted so the
// inviter can see what happened, and because idx_invites_open is partial on
// status='pending' a fresh invitation can still be sent later.
func (s *Store) DeclineInvite(ctx context.Context, inviteID int64, email string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE household_invites
		SET status = 'declined', responded_at = datetime('now')
		WHERE id = ? AND status = 'pending' AND email = ? COLLATE NOCASE`,
		inviteID, NormalizeEmail(email))
	if err != nil {
		return fmt.Errorf("decline invite: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeInvite withdraws an invitation the household sent.
func (s *Store) RevokeInvite(ctx context.Context, inviteID, householdID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE household_invites
		SET status = 'revoked', responded_at = datetime('now')
		WHERE id = ? AND household_id = ? AND status = 'pending'`,
		inviteID, householdID)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
