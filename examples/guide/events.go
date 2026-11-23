package guide

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Fulfil applies completed orders from the event log until it is caught up.
func Fulfil(ctx context.Context, shop *openrails.Client, db *pgxpool.Pool) error {
	// guide:events
	for {
		cursor, err := loadCursor(ctx, db) // "" starts at the oldest retained event
		if err != nil {
			return err
		}
		page, err := shop.ListEvents(ctx, billing.EventListParams{After: cursor, Limit: 100,
			Types: []string{string(billing.HostEventOrderCompleted)}})
		var status *billing.StatusError
		if errors.As(err, &status) && status.Code == billing.CodeEventCursorExpired {
			return rebuild(ctx, db) // 410: re-derive your state, then start a new cursor
		}
		if err != nil {
			return err
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		for _, event := range page.Events {
			if err := fulfil(ctx, tx, event); err != nil { // idempotent by event.ID
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err := saveCursor(ctx, tx, page.NextCursor); err != nil { // with the effects, empty pages too
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil || !page.HasMore {
			return err
		}
	}
	// guide:end
}

func loadCursor(ctx context.Context, db *pgxpool.Pool) (string, error) {
	var cursor string
	err := db.QueryRow(ctx, `SELECT cursor FROM app.billing_cursor WHERE id = 1`).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func saveCursor(ctx context.Context, tx pgx.Tx, cursor string) error {
	_, err := tx.Exec(ctx, `INSERT INTO app.billing_cursor (id, cursor) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET cursor = excluded.cursor`, cursor)
	return err
}

// fulfil grants what the order bought, once per event.
func fulfil(ctx context.Context, tx pgx.Tx, event billing.Event) error {
	_, err := tx.Exec(ctx, `INSERT INTO app.fulfilled_orders (event_id, order_id) VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING`, event.ID, event.Subject.ID)
	return err
}

// rebuild forgets the cursor; the app re-reads the orders it derives from.
func rebuild(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `DELETE FROM app.billing_cursor`)
	return err
}
