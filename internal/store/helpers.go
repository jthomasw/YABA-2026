package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ── scanning helpers ──────────────────────────────────────────────────────────

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransaction(row rowScanner) (Transaction, error) {
	var t Transaction
	var kind string
	var amount int64
	var essential sql.NullInt64
	var fundID, bucketID sql.NullInt64

	// The order here must match txSelect exactly.
	err := row.Scan(&t.ID, &kind, &t.Label, &amount, &t.OccurredOn, &essential,
		&t.Payee, &t.Place, &t.Note, &bucketID, &t.BucketName,
		&fundID, &t.FundName, &t.ReceiptPath, &t.ReceiptName, &t.CreatedAt,
		&t.AddedBy, &t.Version)
	if err != nil {
		return Transaction{}, err
	}
	if bucketID.Valid {
		v := bucketID.Int64
		t.BucketID = &v
	}

	t.Kind = Kind(kind)
	t.Amount = Cents(amount)
	if essential.Valid {
		v := essential.Int64 == 1
		t.Essential = &v
	}
	if fundID.Valid {
		v := fundID.Int64
		t.FundID = &v
	}
	return t, nil
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// cleanLabel trims a label and caps its length.
func cleanLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// Truncate by rune, not byte: slicing a byte string mid-character would
	// emit invalid UTF-8 and render as a replacement glyph.
	const max = 60
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// escapeLike neutralises LIKE metacharacters in user search text.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// resolveBucket validates a bucket id supplied by a form: nil for no bucket, or the id.
func (s *Store) resolveBucket(ctx context.Context, sc Scope, bucketID *int64) (any, error) {
	if bucketID == nil || *bucketID <= 0 {
		return nil, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM expense_buckets
		 WHERE id = ? AND household_id = ? AND archived_at IS NULL`,
		*bucketID, sc.HouseholdID).Scan(&n)
	if err != nil {
		return nil, fmt.Errorf("verify bucket: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return *bucketID, nil
}

// cleanNote trims a free-text note and caps it.
func cleanNote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	const max = 500
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}
