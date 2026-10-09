package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── login rate limiting ───────────────────────────────────────────────────────

// RateWindow and RateMaxTries define the login limit.
//
// RateBurstTries is the separate ceiling for a whole-IP counter. It is several
// times RateMaxTries on purpose: ten failures against one address is somebody
// guessing at that account, but sixty failures from one address spread over
// many accounts is a password spray, and a shared office or campus network has
// to be able to mistype passwords all morning without the building losing
// access.
const (
	RateWindow     = 10 * time.Minute
	RateMaxTries   = 10
	RateBurstTries = 60
)

// RateLimit is one counter's budget: at most Max events per Window. The window
// is fixed, starting at the first event, and resets once it has passed.
type RateLimit struct {
	Max    int64
	Window time.Duration
}

// The limits that are not password guesses.
var (
	// SignupLimit is how many accounts one network address may create. Enough
	// for a household setting up together, too few to mass-produce accounts.
	SignupLimit = RateLimit{Max: 5, Window: time.Hour}

	// InviteLimit is how many invitation emails one account may cause (new
	// invitations and resends together) per day. Without it, anybody could
	// register and use this server's mail relay to send unlimited email.
	InviteLimit = RateLimit{Max: 20, Window: 24 * time.Hour}

	// InviteRecipientLimit is how many invitations one address may receive per
	// day, from anybody, so it cannot be flooded from many accounts at once.
	InviteRecipientLimit = RateLimit{Max: 3, Window: 24 * time.Hour}
)

// longestRateWindow is how long PurgeOldAttempts keeps a counter. It must be
// at least the longest Window above, or a day-long limit would be forgotten
// after ten minutes.
const longestRateWindow = 24 * time.Hour

// RateRetryIn reports how long a key must wait, or zero if it may try now.
func (s *Store) RateRetryIn(ctx context.Context, key string) (time.Duration, error) {
	return s.RateRetryInMax(ctx, key, RateMaxTries)
}

// RateRetryInMax is RateRetryIn against a caller-chosen budget, for counters
// that are not one-per-account.
func (s *Store) RateRetryInMax(ctx context.Context, key string, maxTries int64) (time.Duration, error) {
	return s.RateRetryInFor(ctx, key, RateLimit{Max: maxTries, Window: RateWindow})
}

// RateRetryInFor reports how long key must wait under limit, or zero if it may
// go ahead now.
func (s *Store) RateRetryInFor(ctx context.Context, key string, limit RateLimit) (time.Duration, error) {
	window := ago(limit.Window)
	ahead := after(limit.Window)

	// Seconds remaining are computed in SQL rather than by parsing the timestamp in Go.
	var failures, remaining int64
	err := s.db.QueryRowContext(ctx, `
		SELECT failures,
		       strftime('%s', window_start, ?) - strftime('%s', 'now')
		FROM login_attempts
		WHERE key = ? AND window_start > datetime('now', ?)`,
		ahead, key, window).Scan(&failures, &remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read rate counter: %w", err)
	}
	if failures < limit.Max {
		return 0, nil
	}
	if remaining <= 0 {
		// The window has just lapsed between the WHERE clause and this line.
		return 0, nil
	}
	return time.Duration(remaining) * time.Second, nil
}

// RateFail records a failed password attempt against the login window.
func (s *Store) RateFail(ctx context.Context, key string) error {
	return s.RateHit(ctx, key, RateLimit{Max: RateMaxTries, Window: RateWindow})
}

// RateHit counts one event against key, starting a new window when the old one
// has aged out. Each key must always be used with the same limit.
func (s *Store) RateHit(ctx context.Context, key string, limit RateLimit) error {
	window := ago(limit.Window)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO login_attempts(key, failures, window_start)
		VALUES(?, 1, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET
			failures = CASE
				WHEN login_attempts.window_start > datetime('now', ?) THEN login_attempts.failures + 1
				ELSE 1
			END,
			window_start = CASE
				WHEN login_attempts.window_start > datetime('now', ?) THEN login_attempts.window_start
				ELSE datetime('now')
			END`, key, window, window)
	if err != nil {
		return fmt.Errorf("record rate event: %w", err)
	}
	return nil
}

// RateTake atomically consumes one slot and reports whether a slot was available.
// Keeping the check and increment in one statement matters for expensive actions
// such as account creation: several concurrent requests must not all observe the
// same remaining slot before any of them records its use.
func (s *Store) RateTake(ctx context.Context, key string, limit RateLimit) (bool, error) {
	window := ago(limit.Window)
	var failures int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO login_attempts(key, failures, window_start)
		VALUES(?, 1, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET
			failures = CASE
				WHEN login_attempts.window_start <= datetime('now', ?) THEN 1
				ELSE login_attempts.failures + 1 END,
			window_start = CASE
				WHEN login_attempts.window_start <= datetime('now', ?) THEN datetime('now')
				ELSE login_attempts.window_start END
		WHERE login_attempts.window_start <= datetime('now', ?)
		   OR login_attempts.failures < ?
		RETURNING failures`, key, window, window, window, limit.Max).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("take rate slot: %w", err)
	}
	return true, nil
}

// RateReset clears the counter after a successful sign-in.
func (s *Store) RateReset(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("reset login attempts: %w", err)
	}
	return nil
}

// PurgeOldAttempts drops windows that have expired, so the table does not grow
// with one row per address that ever mistyped a password.
func (s *Store) PurgeOldAttempts(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_attempts WHERE window_start <= datetime('now', ?)`, ago(longestRateWindow))
	if err != nil {
		return 0, fmt.Errorf("purge login attempts: %w", err)
	}
	return res.RowsAffected()
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique")
}
