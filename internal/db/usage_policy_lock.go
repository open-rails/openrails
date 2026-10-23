package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// LockUsagePolicy serializes merchant defaults with customer overrides. The
// external configuration write may need database connections of its own, so
// this session never consumes a slot in the caller's pool or holds a SQL
// transaction open. Closing it releases the advisory lock even after failure.
func (d *DB) LockUsagePolicy(ctx context.Context, merchant billing.MerchantID) (func(), error) {
	if d == nil || d.pool == nil || merchant.IsZero() {
		return nil, fmt.Errorf("usage policy lock requires a pool and merchant")
	}
	acquire, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(acquire, d.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, fmt.Errorf("connect usage policy lock: %w", err)
	}
	close := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}
	key := "openrails:usage-policy:" + d.rw.schema() + ":" + merchant.String()
	if err := gen.New(conn).LockUsagePolicy(acquire, key); err != nil {
		close()
		return nil, fmt.Errorf("lock usage policy: %w", err)
	}
	return close, nil
}
