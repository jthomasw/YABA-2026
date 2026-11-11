package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// ── passwd ────────────────────────────────────────────────────────────────────

func (a *app) runPasswd(dbPath, email string, list bool) error {
	// No lock: the change is one SQLite transaction, safe beside the running server,
	// and resetting a password is often needed precisely while it is serving.
	if err := requireDB(dbPath); err != nil {
		return err
	}
	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// Deliberately does not run migrations: upgrading a schema as a side effect of a
	// password reset would be surprising and hard to undo.

	if list {
		return a.listAccounts(sqlDB)
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("give an account with -email, or use -list to see them")
	}

	var id int64
	var current string
	err = sqlDB.QueryRow(`
		SELECT id, IFNULL(email, username) FROM users
		WHERE email = ? COLLATE NOCASE OR username = ? COLLATE NOCASE
		LIMIT 1`, email, email).Scan(&id, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account matches %q (try -list)", email)
	}
	if err != nil {
		return fmt.Errorf("look up account: %w", err)
	}

	fmt.Fprintf(a.out, "Setting a new password for %s (id %d).\n", current, id)
	password, err := a.promptSecret("New password: ")
	if err != nil {
		return err
	}
	again, err := a.promptSecret("Confirm password: ")
	if err != nil {
		return err
	}
	if password != again {
		return errors.New("the two passwords do not match")
	}

	if err := checkPassword(password); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// The password and the session revocation share one transaction.
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), id)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("expected to update 1 row, updated %d", n)
	}

	// Every device holding a session is signed out, which is what makes a password change
	// a real response to a suspected compromise.
	revoked, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	n, _ := revoked.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM password_resets WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("revoke password reset links: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Fprintf(a.out, "Password updated for %s.\n", current)
	switch n {
	case 0:
		fmt.Fprintln(a.out, "No active logins to sign out.")
	case 1:
		fmt.Fprintln(a.out, "Signed out 1 active login. It will be asked to log in again.")
	default:
		fmt.Fprintf(a.out, "Signed out %d active logins. They will be asked to log in again.\n", n)
	}
	return nil
}

func (a *app) listAccounts(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`
		SELECT id, IFNULL(email, username), IFNULL(display_name, ''), created_at
		FROM users ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	fmt.Fprintf(a.out, "%-5s %-34s %-16s %s\n", "ID", "EMAIL", "NAME", "CREATED")
	for rows.Next() {
		var id int64
		var email, name, created string
		if err := rows.Scan(&id, &email, &name, &created); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%-5d %-34s %-16s %s\n", id, email, name, created)
	}
	return rows.Err()
}

// checkPassword applies the same rules the signup form does, so a password set
// here cannot be one the application would have refused.
func checkPassword(p string) error {
	if len(p) < 8 {
		return errors.New("passwords need at least 8 characters")
	}
	// bcrypt truncates silently at 72 bytes, so a longer password would be
	// accepted here and then only partly checked at login.
	if len(p) > 72 {
		return errors.New("passwords can be at most 72 characters")
	}
	if strings.TrimSpace(p) == "" {
		return errors.New("that password is only whitespace")
	}
	return nil
}

// promptSecret reads a password from stdin. At a terminal, echo is switched off
// while it is typed, so it is not left on the screen; piped input is read as is.
func (a *app) promptSecret(label string) (string, error) {
	fmt.Fprint(a.out, label)
	restore := echoOff(a.term)
	line, err := a.in.ReadString('\n')
	if restore != nil {
		restore()
		fmt.Fprintln(a.out) // the Enter that was typed was not echoed either
	}
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
