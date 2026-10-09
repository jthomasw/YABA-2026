package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// User is an account. It never carries the password hash.
type User struct {
	ID          int64
	Email       string
	DisplayName string
}

// Name is what the UI greets the user with.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	// Fall back to the part before the @, so the header reads "kushith" rather
	// than the full address.
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return u.Email
}

// NormalizeEmail lowercases and trims a login identifier so that
// "Bob@X.com " and "bob@x.com" cannot become two accounts.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// CreateUser inserts a new account, with display_name derived from the address because
// signup asks only for an email and a password.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (int64, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return 0, fmt.Errorf("email is required")
	}

	display := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		display = email[:i]
	}

	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM users
			WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE`,
			email, email).Scan(&n); err != nil {
			return fmt.Errorf("check existing account: %w", err)
		}
		if n > 0 {
			return ErrEmailTaken
		}

		// username is written as well as email: a vestige of the old schema that migration 3
		// deliberately did not drop, because rebuilding the table would have cascaded and
		// deleted every transaction. It is NOT NULL UNIQUE, so it still needs a value.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users(username, email, display_name, password_hash) VALUES(?, ?, ?, ?)`,
			email, email, display, passwordHash)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}

		// Every account owns a personal household from the moment it exists, in the same
		// transaction as the insert: a user committed without one could sign in and have
		// nowhere to record anything, and every page would have to cope with that state.
		_, err = createPersonalHousehold(ctx, tx, id, display)
		return err
	})

	return id, err
}

// CredentialsFor looks up an account and returns its password hash, matching exactly
// first and case-insensitively second.
func (s *Store) CredentialsFor(ctx context.Context, email string) (User, string, error) {
	typed := strings.TrimSpace(email)
	u, hash, err := s.credentials(ctx, typed, "")
	if errors.Is(err, ErrNotFound) {
		// Case-insensitive second. LIMIT by lowest id keeps the result
		// deterministic if an ambiguous legacy pair is somehow reached from here.
		u, hash, err = s.credentials(ctx, NormalizeEmail(typed), " COLLATE NOCASE")
	}
	return u, hash, err
}

// credentials looks an account up by email or legacy username, with the
// collation given ("" for exact, " COLLATE NOCASE" for case-insensitive).
func (s *Store) credentials(ctx context.Context, ident, collate string) (User, string, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), password_hash
		FROM users
		WHERE email = ?`+collate+` OR username = ?`+collate+`
		ORDER BY id ASC
		LIMIT 1`, ident, ident).Scan(&u.ID, &u.Email, &u.DisplayName, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("select user: %w", err)
	}
	return u, hash, nil
}

// EmailExists reports whether an address already has an account, which is how the
// combined login and signup form decides which of the two the user is doing.
func (s *Store) EmailExists(ctx context.Context, email string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users
		WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE`,
		NormalizeEmail(email), NormalizeEmail(email)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("email exists: %w", err)
	}
	return n > 0, nil
}
