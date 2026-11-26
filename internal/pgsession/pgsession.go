// Package pgsession dials a PostgreSQL session of its own from a pool
// configuration, for what a transaction pooler cannot carry: LISTEN and
// session advisory locks.
package pgsession

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dial connects with cfg's connection settings and its BeforeConnect and
// AfterConnect hooks. cfg must reach PostgreSQL directly. The caller closes
// the connection.
func Dial(ctx context.Context, cfg *pgxpool.Config) (*pgx.Conn, error) {
	connConfig := cfg.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, err
		}
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
