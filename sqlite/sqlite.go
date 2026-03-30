package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"myapp/errs"
	"myapp/user"

	_ "modernc.org/sqlite"
)

// Client wraps the SQLite connection and satisfies user.UserRepository.
type Client struct {
	db *sql.DB
}

func NewClient(dataSourceName string) (Client, error) {
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return Client{}, fmt.Errorf("could not open database: %w", err)
	}
	if err = db.Ping(); err != nil {
		return Client{}, fmt.Errorf("could not reach database: %w", err)
	}
	return Client{db: db}, nil
}

// Migrate creates the users table if it does not already exist.
func (c *Client) Migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS users (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password TEXT NOT NULL
	);`
	if _, err := c.db.Exec(query); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	return nil
}

// StoreUser satisfies user.UserRepository.
// Note: sqlite imports user, not the other way around — dependency inversion.
func (c *Client) StoreUser(ctx context.Context, username, hashedPassword string) error {
	_, err := c.db.ExecContext(
		ctx,
		"INSERT INTO users(username, password) VALUES(?, ?)",
		username, hashedPassword,
	)
	if err != nil {
		// Surface a domain-friendly error for duplicate usernames.
		return errs.BadRequest("username already taken")
	}
	return nil
}

// GetUserByUsername satisfies user.UserRepository.
func (c *Client) GetUserByUsername(ctx context.Context, username string) (user.User, error) {
	row := c.db.QueryRowContext(
		ctx,
		"SELECT id, username, password FROM users WHERE username = ?",
		username,
	)
	var u user.User
	err := row.Scan(&u.ID, &u.Username, &u.Password)
	if err == sql.ErrNoRows {
		return user.User{}, errs.Unauthorized("invalid username or password")
	}
	if err != nil {
		return user.User{}, fmt.Errorf("could not scan user row: %w", err)
	}
	return u, nil
}
