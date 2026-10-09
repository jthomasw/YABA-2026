package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ── audit log ─────────────────────────────────────────────────────────────────

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID        int64
	Actor     string // display name or email; "a removed account" if the user is gone
	Action    string // created, edited, deleted, deposited, withdrew, ...
	Entity    string // transaction, fund, member, invitation, budget
	EntityID  *int64
	Summary   string
	CreatedAt string
}

// recordAudit writes one entry inside the caller's transaction, deliberately not its
// own.
func recordAudit(ctx context.Context, tx *sql.Tx, sc Scope,
	action, entity string, entityID int64, summary string) error {
	var id any
	if entityID != 0 {
		id = entityID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log(household_id, user_id, action, entity, entity_id, summary)
		VALUES(?, ?, ?, ?, ?, ?)`,
		sc.HouseholdID, sc.UserID, action, entity, id, summary)
	if err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// AuditLog returns a household's recent history, newest first.
func (s *Store) AuditLog(ctx context.Context, sc Scope, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id,
		       IFNULL(NULLIF(u.display_name, ''), IFNULL(u.email, u.username)),
		       a.action, a.entity, a.entity_id, a.summary, a.created_at
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.user_id
		WHERE a.household_id = ?
		ORDER BY a.id DESC
		LIMIT ?`, sc.HouseholdID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var actor sql.NullString
		if err := rows.Scan(&e.ID, &actor, &e.Action, &e.Entity,
			&e.EntityID, &e.Summary, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		// The account was deleted. The record of what it did survives, which is
		// the point of ON DELETE SET NULL on that column.
		e.Actor = "a removed account"
		if actor.Valid && actor.String != "" {
			e.Actor = actor.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
