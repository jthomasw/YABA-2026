// Package db opens and configures the SQLite database.
package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Open connects to the SQLite file at path with the pragmas this app needs: WAL so a
// reader cannot block a writer, a busy timeout so a contended write waits rather than
// failing instantly, and foreign keys on (SQLite defaults them off).
func Open(path string) (*sql.DB, error) {
	// Pragmas in the DSN are applied to every connection in the pool, which
	// matters because setting them with a one-off Exec only affects whichever
	// pooled connection happened to serve that call.
	dsn := path + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// SQLite allows one writer at a time. A single pooled connection turns lock
	// contention into harmless queueing inside the process instead of SQLITE_BUSY.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return sqlDB, nil
}

// OpenReadOnly opens a second, read-only handle on the database at path, for
// taking backups.
//
// The server's own handle is capped at one connection, so a backup run through
// it -- VACUUM INTO reads the whole database -- held up every request until it
// finished. In WAL mode a separate reader does not block the writer, nor the
// writer it, so snapshots no longer freeze the site.
func OpenReadOnly(path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q read-only: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %q read-only: %w", path, err)
	}
	return sqlDB, nil
}

// fileURI turns a filesystem path into a SQLite "file:" URI, escaping the
// characters a URI would otherwise read as syntax.
func fileURI(path string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23", " ", "%20")
	return "file:" + r.Replace(filepath.ToSlash(path))
}
