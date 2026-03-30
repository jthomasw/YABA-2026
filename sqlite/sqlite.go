package sqlite

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/jthomasw/YABA-2026/foo"
)

type Client struct {
	client *sqlx.DB
}

func NewClient(connectionString string) (Client, error) {
	db, err := sqlx.Connect("sqlite", connectionString)
	if err != nil {
		return Client{}, fmt.Errorf("could not connect to database: %w", err)
	}
	return Client{
		client: db,
	}, nil
}

// GetBarById fulfills one of the functions for the interface signature defined in the foo package for the BarRepository
// Take note here, an important thing is happening.
// Database code is importing 'business logic code' instead of business logic code importing database code.
// This is the mythical dependency inversion you might have heard about.
func (database *Client) GetBarById(ctx context.Context, id string) (foo.Bar, error) {
	rows, err := database.client.QueryContext(ctx, `select * from bar where id = ?`, id)
	if err != nil {
		return foo.Bar{}, fmt.Errorf("could not perform query: %w", err)
	}
	defer rows.Close()
	var bar foo.Bar
	for rows.Next() {
		err = rows.Scan(&bar.Id, &bar.B, &bar.A, &bar.R)
		if err != nil {
			return foo.Bar{}, fmt.Errorf("could not scan row: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return foo.Bar{}, fmt.Errorf("could not scan rows: %w", err)
	}
	return bar, nil
}

// StoreBar fulfills the other function for the interface signature defined in the foo package for the BarRepository
// Take note here, an important thing is happening.
// Database code is importing 'business logic code' instead of business logic code importing database code.
// This is the mythical dependency inversion you might have heard about.
func (database *Client) StoreBar(ctx context.Context, bar foo.Bar) error {
	_, err := database.client.ExecContext(ctx, `insert into bar(id, b, a, r) values (?, ?, ?, ?)`, bar.Id, bar.B, bar.A, bar.R)
	if err != nil {
		return fmt.Errorf("error executing insert query: %w", err)
	}
	return nil
}
