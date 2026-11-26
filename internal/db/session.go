package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/pgidentity"
	"github.com/open-rails/openrails/internal/pgsession"
)

// SessionConfig is the direct configuration sessions are dialed from when
// the query pool is a transaction pooler; nil when the pool is direct.
func (d *DB) SessionConfig() *pgxpool.Config {
	if d == nil || d.direct == nil {
		return nil
	}
	return d.direct.Copy()
}

// ConnectSession dials a dedicated connection for a session-scoped operation
// (a LISTEN, a session advisory lock): from the direct configuration when the
// query pool is a transaction pooler, else from the pool's. The caller closes
// it when the operation ends.
func (d *DB) ConnectSession(ctx context.Context) (*pgx.Conn, error) {
	if d == nil || d.pool == nil {
		return nil, errors.New("db: opening a session requires a pool-backed DB")
	}
	return pgsession.Dial(ctx, d.SessionDialConfig())
}

// SessionDialConfig is what ConnectSession dials: the direct configuration
// when the pool is a transaction pooler, else the pool's.
func (d *DB) SessionDialConfig() *pgxpool.Config {
	if d.direct != nil {
		return d.direct.Copy()
	}
	return d.pool.Config()
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
