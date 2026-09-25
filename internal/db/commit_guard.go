package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommitGuard runs inside a transaction just before it commits; an error
// rolls the transaction back. A claim holder uses it so that no transaction
// it opens commits after it lost the claim (#1099).
type CommitGuard func(ctx context.Context, tx pgx.Tx) error

type commitGuardKey struct{}

// WithCommitGuard adds guard to every transaction begun through this package
// with ctx.
func WithCommitGuard(ctx context.Context, guard CommitGuard) context.Context {
	if prev, ok := ctx.Value(commitGuardKey{}).(CommitGuard); ok {
		next := guard
		guard = func(ctx context.Context, tx pgx.Tx) error {
			if err := prev(ctx, tx); err != nil {
				return err
			}
			return next(ctx, tx)
		}
	}
	return context.WithValue(ctx, commitGuardKey{}, guard)
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if guard, ok := ctx.Value(commitGuardKey{}).(CommitGuard); ok {
		if err := guard(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SeparatePool opens a new pool of at most maxConns connections with this
// database's connection settings and schema, for work that must not queue
// behind the main pool. The caller closes it.
func (d *DB) SeparatePool(ctx context.Context, maxConns int32) (*DB, error) {
	if d == nil || d.pool == nil {
		return nil, fmt.Errorf("db: a separate pool needs a pool-backed DB")
	}
	cfg := d.pool.Config()
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DB{river: d.river, pool: pool, rw: d.rw, ownsPool: true}, nil
}
