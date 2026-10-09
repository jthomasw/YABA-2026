package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Notification is a message waiting for a user, stored in a table rather than the
// session: an unseen row keeps waiting however long the user stays away, and survives
// a server restart.
type Notification struct {
	ID        int64
	Kind      string // "info" | "success" | "error"
	Text      string
	Link      string
	CreatedAt string
}

// Notify records a message for a user.
func (s *Store) Notify(ctx context.Context, userID int64, kind, text, link string) error {
	switch kind {
	case "info", "success", "error":
	default:
		kind = "info"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications(user_id, kind, text, link) VALUES(?, ?, ?, ?)`,
		userID, kind, text, link)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// How long housekeeping keeps rows nobody needs any more.
const (
	// SeenNotificationTTL: a toast that was shown is history after a month.
	SeenNotificationTTL = 30 * 24 * time.Hour
	// UnseenNotificationTTL: one never shown (the user stopped signing in)
	// stops being news after three months.
	UnseenNotificationTTL = 90 * 24 * time.Hour
	// AuditTTL keeps a little over a year of "who changed what", enough to
	// answer questions about the last tax year without the table growing forever.
	AuditTTL = 400 * 24 * time.Hour
)

// PurgeOldNotifications removes notifications that were shown long ago, or were
// never shown and have gone stale.
func (s *Store) PurgeOldNotifications(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM notifications
		WHERE (seen_at IS NOT NULL AND seen_at <= ?)
		   OR created_at <= datetime('now', ?)`,
		time.Now().Add(-SeenNotificationTTL).UTC().Format(time.RFC3339),
		ago(UnseenNotificationTTL))
	if err != nil {
		return 0, fmt.Errorf("purge notifications: %w", err)
	}
	return res.RowsAffected()
}

// PurgeOldAudit removes history entries older than AuditTTL.
func (s *Store) PurgeOldAudit(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM audit_log WHERE created_at <= datetime('now', ?)`, ago(AuditTTL))
	if err != nil {
		return 0, fmt.Errorf("purge audit log: %w", err)
	}
	return res.RowsAffected()
}

// TakeNotifications returns a user's unseen messages and marks them seen.
func (s *Store) TakeNotifications(ctx context.Context, userID int64) ([]Notification, error) {
	out := []Notification{}

	// Reading and marking seen are one transaction, because "take" is the whole
	// contract: two pages loading together -- a tab and its own refresh -- would
	// otherwise both read the same unseen rows before either marked them, and
	// the user would get the same "your receipt is ready" toast twice.
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, kind, text, link, created_at
			FROM notifications
			WHERE user_id = ? AND seen_at IS NULL
			ORDER BY id ASC
			LIMIT 20`, userID)
		if err != nil {
			return fmt.Errorf("read notifications: %w", err)
		}
		var ids []any
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.Kind, &n.Text, &n.Link, &n.CreatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan notification: %w", err)
			}
			out = append(out, n)
			ids = append(ids, n.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}

		ph := make([]byte, 0, len(ids)*2)
		for i := range ids {
			if i > 0 {
				ph = append(ph, ',')
			}
			ph = append(ph, '?')
		}
		// user_id in the WHERE as well as the ids: the ids came from a scoped
		// read, so this changes nothing today, but it means a future caller
		// cannot turn this into a way to mark somebody else's rows seen.
		args := append([]any{time.Now().UTC().Format(time.RFC3339)}, ids...)
		args = append(args, userID)

		if _, err := tx.ExecContext(ctx,
			`UPDATE notifications SET seen_at = ? WHERE id IN (`+string(ph)+`) AND user_id = ?`,
			args...); err != nil {
			return fmt.Errorf("mark notifications seen: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
