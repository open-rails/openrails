package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/pgidentity"
)

// UsesTransactionPooler reports whether the query pool uses transaction pooling.
func (d *DB) UsesTransactionPooler() bool { return d.direct != nil }

// ConnectSession dials a dedicated connection for a session-scoped operation.
// It uses the host's direct configuration when the query pool is pooled by
// transaction. The caller closes the connection when the operation ends.
func (d *DB) ConnectSession(ctx context.Context) (*pgx.Conn, error) {
	return d.connectSession(ctx, nil)
}

func (d *DB) connectSession(ctx context.Context, configure func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	if d == nil || d.pool == nil {
		return nil, errors.New("db: opening a session requires a pool-backed DB")
	}
	cfg := d.direct
	if cfg == nil {
		cfg = d.pool.Config()
	}
	connConfig := cfg.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, err
		}
	}
	if configure != nil {
		configure(connConfig)
	}
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, err
	}
	if cfg.AfterConnect != nil {
		if err := cfg.AfterConnect(ctx, conn); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return conn, nil
}

// MigrationPool supplies direct sessions to the migrator without changing
// ownership of the application pool. release closes only the temporary pool.
func (d *DB) MigrationPool(ctx context.Context) (*pgxpool.Pool, func(), error) {
	if d.direct == nil {
		return d.pool, func() {}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, d.direct.Copy())
	if err != nil {
		return nil, nil, err
	}
	if err := pgidentity.RequireSameDatabase(ctx, d.pool, pool); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("direct Postgres session database: %w", err)
	}
	return pool, pool.Close, nil
}
