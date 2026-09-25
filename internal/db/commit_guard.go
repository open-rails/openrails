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

// ReadSnapshot runs fn in a read-only REPEATABLE READ transaction: every read
// in fn sees one snapshot.
func (d *DB) ReadSnapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return err
	}
	if err := fn(transactionContext(ctx), tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleasePin returns the request's pinned merchant connection to the pool
// while it is idle, for a caller about to wait; the next query pins again, on
// a possibly different connection. Only safe when nothing from before depends
// on that connection: no open transaction, no raw connection handed out, and
// no session-level lock or setting other than the merchant GUC.
func (d *DB) ReleasePin(ctx context.Context) {
	lc, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn)
	if !ok || d == nil || lc.pool != d.pool {
		return
	}
	lc.releaseIdle()
}
