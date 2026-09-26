package db

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Connection discipline (#1105). A request pins one connection (WithMerchantConn)
// and every pool-backed statement it runs reuses that pin while it is idle,
// so a request never waits on the pool for a second connection while holding
// one: at concurrency >= pool size that wait was a deadlock. Every wait for a
// pooled connection is bounded: a saturated pool answers ErrPoolExhausted
// (503), never a hang.

// poolAcquireTimeout bounds one wait for a pooled connection.
const poolAcquireTimeout = 5 * time.Second

// ErrPoolExhausted is a request that could not get a database connection in
// time. It is retryable.
var ErrPoolExhausted = apperr.New(http.StatusServiceUnavailable, "database_busy", "no database connection is available; retry shortly")

// acquire takes a pooled connection, waiting at most poolAcquireTimeout.
func acquire(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, poolAcquireTimeout)
	defer cancel()
	conn, err := pool.Acquire(actx)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %v", ErrPoolExhausted, err)
	}
	return nil, err
}

// requestConn is the request's pinned connection on pool when it can take a
// statement now: pinned to this pool and schema for the context's merchant,
// not busy with an open result, and not inside a transaction (whose scope a
// pool statement must not join). Otherwise the statement takes a fresh
// connection, whose wait is bounded.
func requestConn(ctx context.Context, pool *pgxpool.Pool, schema string) (*lazyMerchantPgxConn, *pgx.Conn) {
	lc, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn)
	if !ok || lc.pool != pool || lc.schema != schema {
		return nil, nil
	}
	if mid, ok := merchant.FromContext(ctx); !ok || mid.String() != lc.tenantID {
		return nil, nil
	}
	conn, err := lc.get(ctx)
	if err != nil || conn.IsClosed() || conn.PgConn().IsBusy() || conn.PgConn().TxStatus() != 'I' {
		return nil, nil
	}
	return lc, conn
}

// pooledDBTX runs statements on the request's pin when it can, else on a
// pooled connection acquired with a bound. directory skips the pin: platform
// directory reads must not carry the request's merchant session state.
type pooledDBTX struct {
	pool      *pgxpool.Pool
	schema    string
	directory bool
}

func (p pooledDBTX) pin(ctx context.Context) (*lazyMerchantPgxConn, *pgx.Conn) {
	if p.directory {
		return nil, nil
	}
	return requestConn(ctx, p.pool, p.schema)
}

func (p pooledDBTX) pinned(ctx context.Context) *pgx.Conn {
	_, conn := p.pin(ctx)
	return conn
}

func (p pooledDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if conn := p.pinned(ctx); conn != nil {
		return conn.Exec(ctx, sql, args...)
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	return conn.Exec(ctx, sql, args...)
}

func (p pooledDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if conn := p.pinned(ctx); conn != nil {
		return conn.Query(ctx, sql, args...)
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return &releasingRows{Rows: rows, conn: conn}, nil
}

func (p pooledDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if conn := p.pinned(ctx); conn != nil {
		return conn.QueryRow(ctx, sql, args...)
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return errRow{err}
	}
	return releasingRow{row: conn.QueryRow(ctx, sql, args...), conn: conn}
}

func (p pooledDBTX) Begin(ctx context.Context) (pgx.Tx, error) {
	if lc, conn := p.pin(ctx); conn != nil {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		lc.mu.Lock()
		lc.poolTx = true
		lc.mu.Unlock()
		return &pinnedPoolTx{Tx: tx, lc: lc}, nil
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return &releasingTx{Tx: tx, conn: conn}, nil
}

// pinnedPoolTx is a pool transaction on the request's pin; while it is open,
// DB statements on the pin are refused rather than joining it.
type pinnedPoolTx struct {
	pgx.Tx
	lc *lazyMerchantPgxConn
}

func (t *pinnedPoolTx) Commit(ctx context.Context) error {
	defer t.done()
	return t.Tx.Commit(ctx)
}

func (t *pinnedPoolTx) Rollback(ctx context.Context) error {
	defer t.done()
	return t.Tx.Rollback(ctx)
}

func (t *pinnedPoolTx) done() {
	t.lc.mu.Lock()
	t.lc.poolTx = false
	t.lc.mu.Unlock()
}

// releasingRows returns its connection once the rows are done.
type releasingRows struct {
	pgx.Rows
	conn *pgxpool.Conn
	once sync.Once
}

func (r *releasingRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.release()
	return false
}

func (r *releasingRows) Close() { r.release() }

func (r *releasingRows) release() {
	r.once.Do(func() {
		r.Rows.Close()
		r.conn.Release()
	})
}

type releasingRow struct {
	row  pgx.Row
	conn *pgxpool.Conn
}

func (r releasingRow) Scan(dest ...any) error {
	defer r.conn.Release()
	return r.row.Scan(dest...)
}

// releasingTx returns its connection when the transaction ends.
type releasingTx struct {
	pgx.Tx
	conn *pgxpool.Conn
	once sync.Once
}

func (t *releasingTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.release()
	return err
}

func (t *releasingTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.release()
	return err
}

func (t *releasingTx) release() { t.once.Do(t.conn.Release) }
