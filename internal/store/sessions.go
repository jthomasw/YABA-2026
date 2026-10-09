package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A login is a row here, not just a signed cookie.

// after and ago render a duration as a SQLite datetime modifier, "+86400 seconds"
// and "-86400 seconds", so every expiry is computed by SQLite in the same clock
// the later comparison uses -- and derived from the Go constant, so the two
// cannot disagree.
func after(d time.Duration) string { return fmt.Sprintf("+%d seconds", int64(d.Seconds())) }
func ago(d time.Duration) string   { return fmt.Sprintf("-%d seconds", int64(d.Seconds())) }

const (
	// SessionTTL is how long a login lasts from the moment it is created, however active
	// it is.
	SessionTTL = 30 * 24 * time.Hour

	// SessionIdleTTL is how long a login may go untouched before it counts as abandoned.
	SessionIdleTTL = 14 * 24 * time.Hour

	// sessionTouchAfter is how stale last_seen_at must be before a request updates it.
	sessionTouchAfter = time.Minute
)

// Session is one active login, as shown on the device list.
type Session struct {
	// ID is the stored hash of the session token (see TokenHash), never the
	// token itself, so it is safe to put in a form on the device list.
	ID         string
	UserID     int64
	CreatedAt  string
	LastSeenAt string
	ExpiresAt  string
	UserAgent  string

	// Current marks the session making the request, so the UI can label it and
	// not offer to sign it out alongside the others.
	Current bool
}

// DeviceName turns the browser's self-description into something recognisable -- Chrome
// on Windows rather than 120 characters of Mozilla/5.0 -- so a login that is not yours
// can be spotted.
func (s Session) DeviceName() string {
	ua := s.UserAgent
	if strings.TrimSpace(ua) == "" {
		return "Unknown device"
	}

	browser := ""
	switch {
	case strings.Contains(ua, "Edg"):
		browser = "Edge"
	case strings.Contains(ua, "OPR"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "SamsungBrowser"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "Firefox"), strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "CriOS"), strings.Contains(ua, "Chrome"),
		strings.Contains(ua, "Chromium"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari"):
		browser = "Safari"
	}

	// iPhone and iPad are checked before Mac, because their user agents contain
	// "like Mac OS X"; Android is checked before Linux for the same reason.
	device := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		device = "iPhone"
	case strings.Contains(ua, "iPad"):
		device = "iPad"
	case strings.Contains(ua, "Android"):
		device = "Android"
	case strings.Contains(ua, "Windows"):
		device = "Windows"
	case strings.Contains(ua, "CrOS"):
		device = "ChromeOS"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		device = "Mac"
	case strings.Contains(ua, "Linux"):
		device = "Linux"
	}

	switch {
	case browser != "" && device != "":
		return browser + " on " + device
	case browser != "":
		return browser
	case device != "":
		return device
	}
	return "Unknown device"
}

// TokenHash is what the database stores for a bearer token -- a session id or a
// password-reset token -- instead of the token itself.
//
// The browser (or the email) holds the token; the table holds only its
// SHA-256. Somebody who obtains a copy of the database or a backup therefore
// cannot sign in as anyone or reset a password with what they find. A plain
// hash is enough: the tokens are 256 random bits, so there is nothing to guess
// and no need for a slow hash.
//
// Exported because the device list identifies sessions by this value, never by
// the token, and because tests need to find a row from a token they hold.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newSessionID returns a fresh 256-bit random token.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanUserAgent trims a browser's self-description to something loggable.
func cleanUserAgent(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// CreateSession issues a login and returns its token.
func (s *Store) CreateSession(ctx context.Context, userID int64, userAgent string) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	ttl := after(SessionTTL)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, user_id, expires_at, user_agent)
		VALUES (?, ?, datetime('now', ?), ?)`,
		TokenHash(id), userID, ttl, cleanUserAgent(userAgent)); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// SessionUser resolves a session token to the account it belongs to.
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	id := TokenHash(token)

	idle := ago(SessionIdleTTL)

	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, '')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = ?
		  AND s.expires_at   > datetime('now')
		  AND s.last_seen_at > datetime('now', ?)`,
		id, idle,
	).Scan(&u.ID, &u.Email, &u.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve session: %w", err)
	}

	stale := ago(sessionTouchAfter)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = datetime('now')
		WHERE id = ? AND last_seen_at < datetime('now', ?)`, id, stale); err != nil {
		// Not fatal: the caller is authenticated either way, and failing the request over a
		// bookkeeping write would be a poor trade.
		return u, nil
	}
	return u, nil
}

// Sessions lists a user's live logins, most recently active first. current is
// the token of the session asking, so it can be marked.
func (s *Store) Sessions(ctx context.Context, userID int64, current string) ([]Session, error) {
	current = TokenHash(current)
	idle := ago(SessionIdleTTL)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, created_at, last_seen_at, expires_at, user_agent
		FROM sessions
		WHERE user_id = ?
		  AND expires_at   > datetime('now')
		  AND last_seen_at > datetime('now', ?)
		ORDER BY last_seen_at DESC`, userID, idle)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.UserID, &v.CreatedAt, &v.LastSeenAt,
			&v.ExpiresAt, &v.UserAgent); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		v.Current = v.ID == current
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteSession revokes the login a token belongs to (signing out). user_id is
// part of the WHERE clause so a guessed token cannot sign somebody else out.
func (s *Store) DeleteSession(ctx context.Context, userID int64, token string) error {
	return s.DeleteSessionByID(ctx, userID, TokenHash(token))
}

// DeleteSessionByID revokes one login by the ID the device list shows (the
// token's hash), for signing out a different device.
func (s *Store) DeleteSessionByID(ctx context.Context, userID int64, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserSessions revokes every login for an account.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOtherSessions revokes every login for an account except the one given.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, TokenHash(keep))
	if err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeExpiredSessions removes rows no login can use.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	idle := ago(SessionIdleTTL)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE expires_at <= datetime('now')
		   OR last_seen_at <= datetime('now', ?)`, idle)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ── password resets ───────────────────────────────────────────────────────────

// ResetTTL is how long a password reset link works.
const ResetTTL = time.Hour

// CreateReset issues a single-use password reset token, deleting every other
// outstanding one for the account: only the most recent link should work, and the
// owner requesting their own invalidates one an attacker asked for.
func (s *Store) CreateReset(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionID() // same generator: 256 bits of crypto/rand
	if err != nil {
		return "", err
	}
	ttl := after(ResetTTL)

	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clear old resets: %w", err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO password_resets(token, user_id, expires_at)
			 VALUES(?, ?, datetime('now', ?))`, TokenHash(token), userID, ttl)
		if err != nil {
			return fmt.Errorf("create reset: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ResetUser resolves a token to the account it belongs to, without consuming it.
func (s *Store) ResetUser(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, IFNULL(u.email, u.username), IFNULL(u.display_name, '')
		FROM password_resets p
		JOIN users u ON u.id = p.user_id
		WHERE p.token = ? AND p.used_at IS NULL AND p.expires_at > datetime('now')`,
		TokenHash(token),
	).Scan(&u.ID, &u.Email, &u.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve reset token: %w", err)
	}
	return u, nil
}

// ConsumeReset sets a new password hash and burns the token.
func (s *Store) ConsumeReset(ctx context.Context, token, passwordHash string) (int64, error) {
	if token == "" {
		return 0, ErrNotFound
	}
	token = TokenHash(token)
	var userID int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE password_resets SET used_at = datetime('now')
			WHERE token = ? AND used_at IS NULL AND expires_at > datetime('now')
			RETURNING user_id`, token).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("consume reset token: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ? WHERE id = ?`,
			passwordHash, userID); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ? AND token <> ?`,
			userID, token); err != nil {
			return fmt.Errorf("clear other resets: %w", err)
		}
		return nil
	})
	return userID, err
}

// ChangePassword replaces the hash for a signed-in user and signs out their other
// devices.
func (s *Store) ChangePassword(ctx context.Context, userID int64, passwordHash, keepSession string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
		if err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ? AND id <> ?`,
			userID, TokenHash(keepSession)); err != nil {
			return fmt.Errorf("revoke other sessions: %w", err)
		}
		// A pending reset link is stale the moment the password changes.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clear resets: %w", err)
		}
		return nil
	})
}

// PurgeExpiredResets removes tokens that can no longer be used.
func (s *Store) PurgeExpiredResets(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM password_resets
		 WHERE expires_at <= datetime('now') OR used_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("purge resets: %w", err)
	}
	return res.RowsAffected()
}
