package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/sirupsen/logrus"
)

// MerchantGUC is the Postgres run-time configuration parameter (GUC) that carries
// the active merchant id for Row Level Security. Migration 050 enables RLS on every
// merchant-owned table with a policy of the form
//
//	merchant_id = nullif(current_setting('app.merchant_id', true), '')::uuid
//
// so a transaction that has set this GUC sees ONLY its own merchant's rows, and a
// transaction that forgot to set it sees NOTHING (fail-closed). For RLS to
// actually enforce, the application must connect as a non-superuser,
// non-BYPASSRLS role (openrails_app, created by 001_schema.up.sql).
const MerchantGUC = "app.merchant_id"

// Qx returns the queryable handle that sqlc-generated queries
// (and the rare annotated raw-pgx escape hatch) MUST run on so the
// migration-050 RLS policies constrain them (issue #227).
//
// Resolution order:
//  1. an open pgx transaction this DB is scoped to (NewWithPgxTx),
//  2. the request's pinned merchant-scoped connection (WithMerchantConn) — it
//     carries the app.merchant_id GUC, so RLS fail-closed semantics apply,
//  3. the base pool (control-plane access to GLOBAL non-RLS tables, or
//     single-merchant/self-hosted before the connection middleware runs).

func (d *DB) Qx(ctx context.Context) gen.DBTX {
	if d == nil {
		return errDBTX{fmt.Errorf("db: Qx on nil DB")}
	}
	// d.pgtx is created via pgxBegin / Pool.Begin, which already return a
	// schema-rewriting tx, so it is returned as-is (no double wrap). The pool and
	// the lazy merchant connection are raw handles, so wrap them here (#471).
	if d.pgtx != nil {
		return d.pgtx
	}
	if lc, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn); ok {
		return d.rw.wrapDBTX(lc)
	}
	if d.pool != nil {
		return d.rw.wrapDBTX(d.pool)
	}
	return errDBTX{fmt.Errorf("db: no pgx handle available on this DB")}
}

// Gen returns the sqlc query catalog bound to Qx(ctx). The standard accessor
// at converted call sites: d.Gen(ctx).SomeQuery(ctx, ...).
func (d *DB) Gen(ctx context.Context) *gen.Queries {
	return gen.New(d.Qx(ctx))
}

// GenDirectory returns a sqlc query catalog bound to the BASE pool, deliberately
// IGNORING any merchant-pinned connection in the context.
//
// It grants NO extra privilege, and its name says so. It was called GenGlobal
// and read as "the cross-merchant accessor"; there is no such thing. There is
// ONE pool and ONE role — dropping the app.merchant_id GUC does not escape RLS,
// it FAILS it. Under the production openrails_app role a base-pool read of any
// policy-bearing table (everything except merchants, probe_verdicts,
// worker_health and destructive_action_switch) matches `merchant_id = NULL` and
// returns ZERO ROWS AND NO ERROR. That false belief is what made the #732
// destructive-rate ceiling, both armed-merchant scans, the #816 re-driver and
// the whole provider-intent plane inert in production (#824/or#860/or#861/
// or#862/or#868).
//
// So exactly two things may run on this handle:
//  1. reads of the four POLICY-FREE tables above, and
//  2. the SECURITY DEFINER directory / scan / work-queue functions of migrations
//     0016, 0021 and 0022 — which RAISE when their definer cannot bypass RLS,
//     so a mis-owned schema fails loudly instead of silently answering nothing.
//
// Anything else that spans merchants is a per-merchant walk: enumerate ids from
// the policy-free merchants directory (or a 0022 work queue), then do the work
// inside each merchant's own scope via RunInMerchantConn/MerchantTx.
//
// Returns an erroring catalog when this DB has no pool (a tx-scoped wrapper).
func (d *DB) GenDirectory() *gen.Queries {
	if d == nil || d.pool == nil {
		return gen.New(errDBTX{fmt.Errorf("db: GenDirectory requires a pool-backed DB")})
	}
	return gen.New(d.rw.wrapDBTX(d.pool))
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
	// Every branch wraps the returned tx so hand-written SQL run on it inside the
	// RunInTx/MerchantTx callback is schema-rewritten (#471). d.pgtx is already a
	// schema-rewriting tx; its nested Begin re-wraps idempotently.
	if d.pgtx != nil {
		// Nested: pgx models nesting as savepoints via tx.Begin.
		return d.pgtx.Begin(ctx)
	}
	if lc, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn); ok {
		// Begin on the pinned merchant connection (acquiring it now if this is
		// the request's first sqlc touch) so the tx inherits the session GUC.
		conn, err := lc.get(ctx)
		if err != nil {
			return nil, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return d.rw.wrapTx(tx), nil
	}
	if d.pool != nil {
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return d.rw.wrapTx(tx), nil
	}
	return nil, fmt.Errorf("db: no pgx handle available to begin transaction (issue #334)")
}

// RunInTx runs fn inside a pgx transaction (no merchant GUC — for control-plane
// and privileged background work that uses explicit merchant_id predicates).
// Begins on the pinned merchant connection
// when one is in flight, so request-path transactions stay RLS-scoped via the
// connection's session GUC.
func (d *DB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MerchantTx runs fn inside a pgx transaction with the RLS merchant GUC pinned
// from the context via set_config(..., is_local=true). Request-path
// merchant-owned writes go through this (or run on a connection pinned by
// WithMerchantConn).
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
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setMerchantLocalGUCPgx sets the merchant GUC transaction-locally
// (set_config is_local=true) so it reverts when the tx ends and can never
// leak onto a pooled connection.
func setMerchantLocalGUCPgx(ctx context.Context, tx pgx.Tx, id merchant.ID) error {
	if id.IsZero() {
		return fmt.Errorf("db: cannot set %s GUC for a zero merchant id", MerchantGUC)
	}
	var out string
	if err := tx.QueryRow(ctx,
		"SELECT set_config($1, $2, TRUE)", MerchantGUC, id.String(),
	).Scan(&out); err != nil {
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
func (d *DB) BindMerchantTx(ctx context.Context, tx pgx.Tx, id merchant.ID) (context.Context, *DB, error) {
	if d == nil {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx on nil DB")
	}
	if tx == nil {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx requires a transaction")
	}
	if id.IsZero() {
		return ctx, nil, fmt.Errorf("db: BindMerchantTx requires a non-zero merchant id")
	}

	var got string
	if err := tx.QueryRow(ctx,
		"SELECT COALESCE(current_setting($1, true), '')", MerchantGUC,
	).Scan(&got); err != nil {
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
	return ctx, d.NewWithPgxTx(tx), nil
}

// lazyMerchantPgxConn is the request's merchant-scoped connection, acquired
// LAZILY on the first query instead of eagerly in WithMerchantConn, so requests
// that never touch the database cost zero pool connections (the eager variant
// collapsed under burst during the #334 transition — see that issue's status
// for the post-mortem).
//
// It satisfies gen.DBTX directly: the first Exec/Query/QueryRow (or
// pgxBegin) acquires a pool connection, sets the app.merchant_id session GUC
// on it, and pins it until release().
type lazyMerchantPgxConn struct {
	pool     *pgxpool.Pool
	tenantID string

	mu   sync.Mutex
	conn *pgxpool.Conn
}

// lazyMerchantAcquireTimeout bounds the lazy acquisition (same rationale as
// tenantConnAcquireTimeout: never park forever on a pool when client aborts
// don't propagate).
const lazyMerchantAcquireTimeout = 4 * time.Second

func (l *lazyMerchantPgxConn) get(ctx context.Context) (*pgxpool.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return l.conn, nil
	}
	acqCtx, cancel := context.WithTimeout(ctx, lazyMerchantAcquireTimeout)
	conn, err := l.pool.Acquire(acqCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("db: acquire pgx merchant connection: %w", err)
	}
	var out string
	if err := conn.QueryRow(ctx,
		"SELECT set_config($1, $2, FALSE)", MerchantGUC, l.tenantID).Scan(&out); err != nil {
		conn.Release()
		return nil, fmt.Errorf("db: set %s on pgx merchant connection: %w", MerchantGUC, err)
	}
	l.conn = conn
	return l.conn, nil
}

// release resets the GUC and returns the connection to the pool (no-op when
// the request never touched a sqlc call site).
func (l *lazyMerchantPgxConn) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	// Background context so release works even after request cancellation.
	// Self-healing (get() re-sets the GUC before any use; RLS fails closed
	// without it), but never return a connection that may still carry a
	// merchant GUC to the pool: on reset failure, warn and close it so the
	// pool destroys it instead of reusing it (#668).
	if _, err := l.conn.Exec(context.Background(),
		"SELECT set_config($1, '', FALSE)", MerchantGUC); err != nil {
		logrus.WithError(err).Warn("db: failed to reset merchant GUC on pgx connection release; discarding connection")
		_ = l.conn.Conn().Close(context.Background())
	}
	l.conn.Release()
	l.conn = nil
}

func (l *lazyMerchantPgxConn) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	conn, err := l.get(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return conn.Exec(ctx, sql, args...)
}

func (l *lazyMerchantPgxConn) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	conn, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Query(ctx, sql, args...)
}

func (l *lazyMerchantPgxConn) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	conn, err := l.get(ctx)
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

// newSQLTracer is the debug-level pgx query tracer, enabled by config
// db.sql_trace (#712; was the ad-hoc OPENRAILS_SQL_TRACE env read).
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
