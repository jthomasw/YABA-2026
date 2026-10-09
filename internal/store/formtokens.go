package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── one-time form tokens ──────────────────────────────────────────────────────

// FormTokenTTL is how long an unused form token stays valid: long enough to fill in a
// form slowly and be interrupted, short enough not to accumulate forgotten tabs.
const FormTokenTTL = 12 * time.Hour

// NewFormToken issues a token to embed in a form that must not be submitted twice.
func (s *Store) NewFormToken(ctx context.Context, userID int64, purpose string) (string, error) {
	token, err := newSessionID()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO form_tokens(token, user_id, purpose) VALUES(?, ?, ?)`,
		token, userID, purpose)
	if err != nil {
		return "", fmt.Errorf("issue form token: %w", err)
	}
	return token, nil
}

// ConsumeFormToken spends a token, reporting whether this caller got it.
func (s *Store) ConsumeFormToken(ctx context.Context, userID int64, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	age := ago(FormTokenTTL)

	var got string
	err := s.db.QueryRowContext(ctx, `
		UPDATE form_tokens SET used_at = datetime('now')
		WHERE token = ? AND user_id = ? AND used_at IS NULL
		  AND created_at > datetime('now', ?)
		RETURNING token`, token, userID, age).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume form token: %w", err)
	}
	return true, nil
}

// RestoreFormToken makes a token usable again when the change it guarded was
// refused before anything was committed.  It is deliberately only used before
// the first successful write; once a row exists, the token stays spent.
func (s *Store) RestoreFormToken(ctx context.Context, userID int64, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE form_tokens SET used_at = NULL
		WHERE token = ? AND user_id = ?`, token, userID)
	if err != nil {
		return fmt.Errorf("restore form token: %w", err)
	}
	return nil
}

// PurgeOldFormTokens drops tokens for forms nobody ever submitted.
func (s *Store) PurgeOldFormTokens(ctx context.Context) (int64, error) {
	age := ago(FormTokenTTL)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM form_tokens WHERE created_at <= datetime('now', ?)`, age)
	if err != nil {
		return 0, fmt.Errorf("purge form tokens: %w", err)
	}
	return res.RowsAffected()
}
