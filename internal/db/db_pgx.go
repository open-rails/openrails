package db

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/sirupsen/logrus"
)

// MerchantGUC carries the selected merchant for stored functions and SQL that
// calls current_merchant_id(). It filters nothing else: tenant isolation is
// the explicit merchant predicate and composite keys.
const MerchantGUC = "openrails.merchant_id"

// Qx returns the handle sqlc queries use: the open transaction this DB is
// scoped to (NewWithPgxTx), else the request's pinned merchant connection,
// else the base pool. Choosing a handle authorizes nothing; tenant queries
// still carry their verified merchant predicate.

func (d *DB) Qx(ctx context.Context) gen.DBTX {
	if d == nil {
		return errDBTX{fmt.Errorf("db: Qx on nil DB")}
	}
	// d.pgtx already rewrites schemas (pgxBegin / Pool.Begin); the pool and
	// the lazy merchant connection are raw, so wrap them here.
	if d.pgtx != nil {
		return d.pgtx
	}
	if lc, err := d.merchantConn(ctx); err != nil {
		return errDBTX{err}
	} else if lc != nil {
		return d.rw.wrapDBTX(lc)
	}
	if d.pool != nil {
		return d.rw.wrapDBTX(pooledDBTX{pool: d.pool, schema: d.rw.schema()})
	}
	return errDBTX{fmt.Errorf("db: no pgx handle available on this DB")}
}

// Gen returns the sqlc query catalog bound to Qx(ctx).
func (d *DB) Gen(ctx context.Context) *gen.Queries {
	return gen.New(d.Qx(ctx))
}

// GenDirectory returns a sqlc query catalog bound to the base pool, ignoring
// any merchant pin in the context. It is only for platform directory,
// coordination and worker discovery, never a fallback for a missing merchant.
// Without a pool (a tx-scoped DB) every call errors.
func (d *DB) GenDirectory() *gen.Queries {
	if d == nil || d.pool == nil {
		return gen.New(errDBTX{fmt.Errorf("db: GenDirectory requires a pool-backed DB")})
	}
	return gen.New(d.rw.wrapDBTX(pooledDBTX{pool: d.pool, directory: true}))
}

// pgxBeginner abstracts where a transaction starts: the pinned merchant
// connection when one is in flight (so the tx inherits its session GUC), the
// base pool otherwise.
type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (d *DB) pgxBegin(ctx context.Context) (pgx.Tx, error) {
	if d == nil {
		return nil, fmt.Errorf("db: transaction on nil DB")
	}
	// Every branch wraps the tx so hand-written SQL in the callback is
	// schema-rewritten; d.pgtx's nested Begin re-wraps idempotently.
	if d.pgtx != nil {
		// Nested: pgx models nesting as savepoints via tx.Begin.
		return d.pgtx.Begin(ctx)
	}
	lc, err := d.merchantConn(ctx)
	if err != nil {
		return nil, err
	}
	if lc != nil {
		// Begin on the pinned merchant connection (acquiring it now if this is
		// the request's first sqlc touch) so the tx inherits the session GUC.
		// Never a second BEGIN inside a transaction already open on it.
		tx, err := lc.begin(ctx)
		if err != nil {
			return nil, err
		}
		return schemaTx{Tx: tx, rw: d.rw, river: d.river}, nil
	}
	if d.pool != nil {
		tx, err := pooledDBTX{pool: d.pool, directory: true}.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return schemaTx{Tx: tx, rw: d.rw, river: d.river}, nil
	}
	return nil, fmt.Errorf("db: no pgx handle available to begin transaction (issue #334)")
}

// RunInTx runs fn in a transaction without setting the merchant GUC, for work
// that uses explicit merchant_id predicates. It begins on the pinned merchant
// connection when one is in flight.
func (d *DB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(transactionContext(ctx), tx); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// MerchantTx runs fn in a transaction with the context's merchant set as a
// transaction-local GUC.
func (d *DB) MerchantTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	id, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setMerchantLocalGUCPgx(ctx, tx, id); err != nil {
		return err
	}
	if err := fn(transactionContext(ctx), tx); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// setMerchantLocalGUCPgx sets the merchant GUC transaction-locally
// (set_config is_local=true) so it reverts when the tx ends and can never
// leak onto a pooled connection.
func setMerchantLocalGUCPgx(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	if id.IsZero() {
		return fmt.Errorf("db: cannot set %s GUC for a zero merchant id", MerchantGUC)
	}
	if _, err := gen.New(tx).SetConfig(ctx, gen.SetConfigParams{Setting: MerchantGUC, Value: id.String(), IsLocal: true}); err != nil {
		return fmt.Errorf("db: set %s GUC: %w", MerchantGUC, err)
	}
	return nil
}

// BindMerchantTx scopes a caller-owned transaction to one merchant without
// taking ownership of its commit or rollback. Embedded hosts use this when one
// transaction must atomically contain both host rows and OpenRails rows.
//
// An already-scoped transaction may be reused only for the same merchant. A
// different existing pin is refused rather than overwritten: changing it
// midway through a shared transaction would make earlier and later statements
// observe different tenants.
func (d *DB) BindMerchantTx(ctx context.Context, tx pgx.Tx, id billing.MerchantID) (context.Context, *DB, error) {
	if d == nil {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx on nil DB")
	}
	if tx == nil {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx requires a transaction")
	}
	if id.IsZero() {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx requires a non-zero merchant id")
	}

	got, err := gen.New(tx).CurrentSetting(ctx, MerchantGUC)
	if err != nil {
		return ctx, nil, fmt.Errorf("db: read %s before binding caller transaction: %w", MerchantGUC, err)
	}
	if got != "" && got != id.String() {
		return ctx, nil, &ErrUnscopedMerchantWork{Op: "BindMerchantTx", Want: id, Got: got}
	}
	if got == "" {
		if err := setMerchantLocalGUCPgx(ctx, tx, id); err != nil {
			return ctx, nil, err
		}
	}
	ctx = merchant.WithID(ctx, id)
	return transactionContext(ctx), d.NewWithPgxTx(tx), nil
}

// lazyMerchantPgxConn is the request's merchant-scoped connection, acquired on
// the first query (not in WithMerchantConn) so requests that never touch the
// database hold no pool connection. Acquiring it sets the
// openrails.merchant_id session GUC; it stays pinned until release().
type lazyMerchantPgxConn struct {
	pool     *pgxpool.Pool
	tenantID string
	schema   string

	mu   sync.Mutex
	conn *pgxpool.Conn
	// poolTx marks a pool transaction open on the pin.
	poolTx bool
}

// get returns the pinned connection, acquiring it on first use. It hands out
// the underlying *pgx.Conn: a caller still holding it after a re-pin gets
// "conn closed" from the dead connection, never a released pool handle.
func (l *lazyMerchantPgxConn) get(ctx context.Context) (*pgx.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		if pinned := l.conn.Conn(); !pinned.IsClosed() || !isDetachedWrite(ctx) {
			return pinned, nil
		}
		// A detached write on a pin its caller's cancellation closed
		// (DetachedWriteContext). Releasing a closed connection destroys it
		// and frees its pool slot before the re-acquire below.
		l.conn.Release()
		l.conn = nil
	}
	conn, err := acquire(ctx, l.pool)
	if err != nil {
		return nil, fmt.Errorf("db: acquire pgx merchant connection: %w", err)
	}
	if _, err := gen.New(conn).SetConfig(ctx, gen.SetConfigParams{Setting: MerchantGUC, Value: l.tenantID}); err != nil {
		conn.Release()
		return nil, fmt.Errorf("db: set %s on pgx merchant connection: %w", MerchantGUC, err)
	}
	l.conn = conn
	return conn.Conn(), nil
}

// release resets the GUC and returns the connection to the pool (no-op when
// the request never touched a sqlc call site).
func (l *lazyMerchantPgxConn) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	if l.conn.Conn().IsClosed() {
		// A closed connection carries no session state and the pool destroys it
		// on release; resetting it would only fail loudly.
		l.conn.Release()
		l.conn = nil
		return
	}
	// Background context so release works after request cancellation. Never
	// pool a connection that may still carry a merchant GUC: on reset failure,
	// close it so the pool destroys it.
	if _, err := gen.New(l.conn).SetConfig(context.Background(), gen.SetConfigParams{Setting: MerchantGUC}); err != nil {
		logrus.WithError(err).Warn("db: failed to reset merchant GUC on pgx connection release; discarding connection")
		_ = l.conn.Conn().Close(context.Background())
	}
	l.conn.Release()
	l.conn = nil
}

// releaseIdle releases the pin when no transaction is open on it.
func (l *lazyMerchantPgxConn) releaseIdle() {
	l.mu.Lock()
	idle := l.conn != nil && !l.conn.Conn().IsClosed() && l.conn.Conn().PgConn().TxStatus() == 'I'
	l.mu.Unlock()
	if idle {
		l.release()
	}
}

// ErrPinInTransaction refuses work on the request's connection that would
// silently join a transaction it did not open: a second BEGIN inside an open
// transaction (whose inner COMMIT would end the outer one early), or a DB
// statement inside a pool transaction. Use that transaction's handle.
var ErrPinInTransaction = errors.New("db: the request's connection is inside a transaction; use that transaction")

// idle returns the pinned connection for a DB statement. A DB transaction open
// on it is joined; a pool transaction open on it is refused.
func (l *lazyMerchantPgxConn) idle(ctx context.Context) (*pgx.Conn, error) {
	conn, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	poolTx := l.poolTx
	l.mu.Unlock()
	if poolTx && conn.PgConn().TxStatus() != 'I' {
		return nil, ErrPinInTransaction
	}
	return conn, nil
}

// begin opens a transaction on the pinned connection, never a second one
// inside a transaction already open on it.
func (l *lazyMerchantPgxConn) begin(ctx context.Context) (pgx.Tx, error) {
	conn, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	if conn.PgConn().TxStatus() != 'I' {
		return nil, ErrPinInTransaction
	}
	return conn.Begin(ctx)
}

func (l *lazyMerchantPgxConn) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	conn, err := l.idle(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return conn.Exec(ctx, sql, args...)
}

func (l *lazyMerchantPgxConn) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	conn, err := l.idle(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Query(ctx, sql, args...)
}

func (l *lazyMerchantPgxConn) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	conn, err := l.idle(ctx)
	if err != nil {
		return errRow{err}
	}
	return conn.QueryRow(ctx, sql, args...)
}

// errDBTX satisfies gen.DBTX but fails every call with a descriptive error.
// Returned by Qx when this DB has no pgx side, so a mis-wired call site
// surfaces as a clear runtime error instead of a nil-pointer panic.
type errDBTX struct{ err error }

func (e errDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, e.err
}
func (e errDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, e.err
}
func (e errDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return errRow{e.err}
}

type errRow struct{ err error }

func (r errRow) Scan(...interface{}) error { return r.err }

// newSQLTracer is the debug-level pgx query tracer, enabled by db.sql_trace.
func newSQLTracer() *tracelog.TraceLog {
	return &tracelog.TraceLog{
		Logger:   tracelog.LoggerFunc(logPGX),
		LogLevel: tracelog.LogLevelDebug,
	}
}

func logPGX(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]interface{}) {
	entry := logrus.WithContext(ctx).WithFields(logrus.Fields(data))
	switch level {
	case tracelog.LogLevelTrace, tracelog.LogLevelDebug:
		entry.Debug(msg)
	case tracelog.LogLevelInfo:
		entry.Info(msg)
	case tracelog.LogLevelWarn:
		entry.Warn(msg)
	default:
		entry.Error(msg)
	}
}
