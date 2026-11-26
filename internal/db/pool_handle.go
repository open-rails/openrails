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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Connection discipline: a statement outside a transaction holds a pooled
// connection only while it runs, and every pool wait is bounded: a saturated
// pool answers ErrPoolExhausted (503), never a hang.

// poolAcquireTimeout bounds one wait for a pooled connection.
const poolAcquireTimeout = 5 * time.Second

// ErrPoolExhausted is a request that could not get a database connection in
// time. It is retryable.
var ErrPoolExhausted = apperr.New(http.StatusServiceUnavailable, billing.CodeDatabaseBusy, "no database connection is available; retry shortly")

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

// pooledDBTX runs each statement on a pooled connection acquired with a
// bound, as the context's merchant scope when it has one for this pool and
// schema. directory ignores the scope: platform directory reads run as no
// merchant.
type pooledDBTX struct {
	pool      *pgxpool.Pool
	schema    string
	directory bool
}

// scoped is the merchant the context scopes statements on this pool to.
func (p pooledDBTX) scoped(ctx context.Context) (string, bool) {
	if p.directory {
		return "", false
	}
	scope, ok := ctx.Value(merchantScopeKey{}).(*merchantScope)
	if !ok || scope.pool != p.pool || scope.schema != p.schema {
		return "", false
	}
	if mid, ok := merchant.FromContext(ctx); !ok || mid.String() != scope.tenantID {
		return "", false
	}
	return scope.tenantID, true
}

func (p pooledDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if id, ok := p.scoped(ctx); ok {
		return scopedStatement{pool: p.pool, merchantID: id}.Exec(ctx, sql, args...)
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	return conn.Exec(ctx, sql, args...)
}

func (p pooledDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if id, ok := p.scoped(ctx); ok {
		return scopedStatement{pool: p.pool, merchantID: id}.Query(ctx, sql, args...)
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
	if id, ok := p.scoped(ctx); ok {
		return scopedStatement{pool: p.pool, merchantID: id}.QueryRow(ctx, sql, args...)
	}
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return errRow{err}
	}
	return releasingRow{row: conn.QueryRow(ctx, sql, args...), conn: conn}
}

// Begin opens a transaction on a connection of its own, the context's
// merchant scope set locally in it.
func (p pooledDBTX) Begin(ctx context.Context) (pgx.Tx, error) {
	conn, err := acquire(ctx, p.pool)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, err
	}
	scoped := &releasingTx{Tx: tx, conn: conn}
	if id, ok := p.scoped(ctx); ok {
		if _, err := gen.New(tx).SetConfig(ctx, gen.SetConfigParams{Setting: MerchantGUC, Value: id, IsLocal: true}); err != nil {
			_ = scoped.Rollback(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("db: set %s GUC: %w", MerchantGUC, err)
		}
	}
	return scoped, nil
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

// scopeStatement sets the merchant for the statement queued after it.
const scopeStatement = "SELECT set_config('" + MerchantGUC + "', $1, true)"

// scopedStatement runs one statement as a merchant outside any transaction:
// the merchant setting and the statement go in one batch, which PostgreSQL
// runs as one implicit transaction, so the setting ends with the statement on
// the same server connection, in one round trip.
type scopedStatement struct {
	pool       *pgxpool.Pool
	merchantID string
}

func (s scopedStatement) send(ctx context.Context, sql string, args []any) (*pgxpool.Conn, pgx.BatchResults, error) {
	conn, err := acquire(ctx, s.pool)
	if err != nil {
		return nil, nil, err
	}
	b := &pgx.Batch{}
	b.Queue(scopeStatement, s.merchantID)
	b.Queue(sql, args...)
	results := conn.SendBatch(ctx, b)
	if _, err := results.Exec(); err != nil {
		_ = results.Close()
		conn.Release()
		return nil, nil, fmt.Errorf("db: set %s GUC: %w", MerchantGUC, err)
	}
	return conn, results, nil
}

func (s scopedStatement) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	conn, results, err := s.send(ctx, sql, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	tag, err := results.Exec()
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	return tag, err
}

func (s scopedStatement) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	conn, results, err := s.send(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	rows, err := results.Query()
	if err != nil {
		_ = results.Close()
		conn.Release()
		return nil, err
	}
	return &batchRows{Rows: rows, results: results, conn: conn}, nil
}

func (s scopedStatement) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	conn, results, err := s.send(ctx, sql, args)
	if err != nil {
		return errRow{err}
	}
	return batchRow{row: results.QueryRow(), results: results, conn: conn}
}

// batchRows ends its batch and returns its connection once the rows are done.
type batchRows struct {
	pgx.Rows
	results pgx.BatchResults
	conn    *pgxpool.Conn
	once    sync.Once
	err     error
}

func (r *batchRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

func (r *batchRows) Close() {
	r.once.Do(func() {
		r.Rows.Close()
		r.err = r.results.Close()
		r.conn.Release()
	})
}

func (r *batchRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.err
}

type batchRow struct {
	row     pgx.Row
	results pgx.BatchResults
	conn    *pgxpool.Conn
}

func (r batchRow) Scan(dest ...any) error {
	defer r.conn.Release()
	err := r.row.Scan(dest...)
	if closeErr := r.results.Close(); err == nil {
		err = closeErr
	}
	return err
}
