// Package store is the only package that speaks SQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
)

// Cents is an alias so callers of this package do not have to import money just to name
// a field type.
type Cents = money.Cents

// Ratio is re-exported for the same reason as Cents.
var Ratio = money.Ratio

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Scope says whose money a call operates on: the household that owns the data, and the
// person performing the action.
type Scope struct {
	HouseholdID int64
	UserID      int64
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Ping checks that the database answers, for the health endpoint.
//
// A real query rather than sql.DB.Ping: the pool is capped at one connection, so
// what an operator needs to know is whether that connection can actually do
// work, not whether the file is still open. A seized writer holding the single
// connection is exactly the failure this has to catch, and it is the one that
// looks identical to a healthy process from outside.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("database health check: %w", err)
	}
	return nil
}

// Sentinel errors. Handlers map these to HTTP responses; they must never leak
// raw SQL text to a user.
var (
	// ErrNotFound covers "no such row" and "that row belongs to someone else".
	ErrNotFound = errors.New("not found")

	// ErrConflict means the row changed between being read and being saved.
	ErrConflict = errors.New("changed by someone else")

	// ErrEmailTaken is returned instead of a raw UNIQUE constraint error.
	ErrEmailTaken = errors.New("that email already has an account")

	// ErrInsufficientCash blocks moving more into a fund than the user holds.
	ErrInsufficientCash = errors.New("not enough available cash")

	// ErrItemsDoNotBalance means a breakdown does not add up to its transaction.
	// It is a sentinel so the handler can re-render the form with the message
	// rather than returning 500 for what is a correctable typing mistake.
	ErrItemsDoNotBalance = errors.New("the line items do not add up to the transaction")

	// ErrInsufficientFund blocks withdrawing more than a fund contains.
	ErrInsufficientFund = errors.New("not enough money in that fund")
)

// clockLocation is the timezone Today() reads the current date in. It defaults
// to the server's own local timezone -- which is what every deployment got
// before this existed -- and is only ever changed once, by SetLocation, before
// the HTTP server starts accepting requests. Nothing after startup mutates it,
// so no mutex guards it: concurrent reads of an unchanging pointer are safe.
var clockLocation = time.Local

// SetLocation points every future Today() call at loc instead of the server's
// local timezone. Call it once at startup, before serving any request -- a
// deployment whose server clock is not in the household's own timezone would
// otherwise file a transaction made just after midnight under the wrong day,
// because "today" was being decided in the wrong place.
func SetLocation(loc *time.Location) {
	if loc != nil {
		clockLocation = loc
	}
}

// Now returns the current instant in clockLocation. Anything that turns "now"
// into a calendar day or month must start from this rather than time.Now():
// the server's own zone can already be in tomorrow, or next month, while the
// household's is not.
func Now() time.Time {
	return timeNow().In(clockLocation)
}

// timeNow is the wall clock behind Now. A variable only so tests can stand on a
// chosen instant (see export_test.go); nothing else assigns it.
var timeNow = time.Now

// Today returns the current date as YYYY-MM-DD, in clockLocation.
func Today() string {
	return Now().Format(DateLayout)
}

// DateLayout is the storage format for occurred_on.
const DateLayout = "2006-01-02"

// MonthLayout is the storage format for a month selector, e.g. "2026-04".
const MonthLayout = "2006-01"

// ParseDate validates a user-supplied date. Without it a hand-edited form could put
// tomorrow, or nothing at all, into a column every chart sorts on.
func ParseDate(s string) (string, error) {
	if s == "" {
		return Today(), nil
	}
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return "", fmt.Errorf("date must look like YYYY-MM-DD")
	}
	// Guard against fat-fingered years such as 20026, which would push a row
	// to the end of every ordered result set forever.
	if y := t.Year(); y < 1900 || y > 2200 {
		return "", fmt.Errorf("date year is out of range")
	}
	return t.Format(DateLayout), nil
}

// monthRange converts 2026-04 into the half-open interval [2026-04-01, 2026-05-01).
func monthRange(month string) (start, end string, err error) {
	if month == "" {
		return "", "", nil
	}
	t, err := time.Parse(MonthLayout, month)
	if err != nil {
		return "", "", fmt.Errorf("month must look like YYYY-MM")
	}
	return t.Format(DateLayout), t.AddDate(0, 1, 0).Format(DateLayout), nil
}

// monthClause is the optional " AND occurred_on >= ? AND occurred_on < ?" that
// narrows a query to one month, with its two bound values; both are empty for
// all time. prefix qualifies the column, e.g. "t.".
func monthClause(month, prefix string) (clause string, args []any, err error) {
	if month == "" {
		return "", nil, nil
	}
	start, end, err := monthRange(month)
	if err != nil {
		return "", nil, err
	}
	return " AND " + prefix + "occurred_on >= ? AND " + prefix + "occurred_on < ?", []any{start, end}, nil
}

// Uncategorised is the group spending falls into when nothing names it: a blank
// label, or a blank line-item category on a transaction with a blank label.
// It is bound into the SQL as a parameter rather than written into it, so this
// is the only place the spelling lives.
const Uncategorised = "Uncategorised"

// LabelTotal is one slice of a breakdown chart.
type LabelTotal struct {
	Label string
	Total Cents
}
