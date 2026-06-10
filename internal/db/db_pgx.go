package db

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/sirupsen/logrus"
)

// Qx is the pgx-side analogue of Q: the handle that sqlc-generated queries
// (and the rare annotated raw-pgx escape hatch) MUST run on so the
// migration-050 RLS policies constrain them (issue #227).
//
// Resolution order:
//  1. an open pgx transaction this DB is scoped to (NewWithPgxTx),
//  2. the request's pinned tenant-scoped connection (WithTenantConn) — it
//     carries the app.tenant_id GUC, so RLS fail-closed semantics apply,
//  3. the base pool (control-plane access to GLOBAL non-RLS tables, or
//     single-tenant/self-hosted before the connection middleware runs).
//
// On a DB with no pgx side (NewWithSQLDB/NewWithBun/bun-tx wrappers) it
// returns a stub whose every call errors loudly — converted code paths must
// only ever be reached with a pgx-backed DB (#334 transition invariant).
func (d *DB) Qx(ctx context.Context) gen.DBTX {
	if d == nil {
		return errDBTX{fmt.Errorf("db: Qx on nil DB")}
	}
	if d.pgtx != nil {
		return d.pgtx
	}
	if c, ok := ctx.Value(tenantPgxConnKey{}).(*pgxpool.Conn); ok {
		return c
	}
	if d.pool != nil {
		return d.pool
	}
	return errDBTX{fmt.Errorf("db: no pgx handle available (DB was built from *sql.DB or a bun tx; sqlc paths need a pgx-backed DB — issue #334)")}
}

// Gen returns the sqlc query catalog bound to Qx(ctx). The standard accessor
// at converted call sites: d.Gen(ctx).SomeQuery(ctx, ...).
func (d *DB) Gen(ctx context.Context) *gen.Queries {
	return gen.New(d.Qx(ctx))
}

// pgxBeginner abstracts where a transaction starts: the pinned tenant
// connection when one is in flight (so the tx inherits its session GUC), the
// base pool otherwise.
type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (d *DB) pgxBegin(ctx context.Context) (pgx.Tx, error) {
	if d == nil {
		return nil, fmt.Errorf("db: transaction on nil DB")
	}
	if d.pgtx != nil {
		// Nested: pgx models nesting as savepoints via tx.Begin.
		return d.pgtx.Begin(ctx)
	}
	var b pgxBeginner
	if c, ok := ctx.Value(tenantPgxConnKey{}).(*pgxpool.Conn); ok {
		b = c
	} else if d.pool != nil {
		b = d.pool
	} else {
		return nil, fmt.Errorf("db: no pgx handle available to begin transaction (issue #334)")
	}
	return b.Begin(ctx)
}

// RunInTx runs fn inside a pgx transaction (no tenant GUC — for control-plane
// and privileged background work that uses explicit tenant_id predicates).
// The pgx analogue of Q(ctx).RunInTx. Begins on the pinned tenant connection
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

// TenantTx runs fn inside a pgx transaction with the RLS tenant GUC pinned
// from the context via set_config(..., is_local=true) — the pgx analogue of
// RunInTenantTx. Request-path tenant-owned writes go through this (or run on
// a connection pinned by WithTenantConn).
func (d *DB) TenantTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	id := tenant.FromContextOrDefault(ctx)
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setTenantLocalGUCPgx(ctx, tx, id); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetTenantGUCPgx pins the RLS tenant GUC (from the context) onto an
// already-open pgx transaction — for callbacks that already received a
// pgx.Tx. Counterpart of SetTenantGUC.
func SetTenantGUCPgx(ctx context.Context, tx pgx.Tx) error {
	return setTenantLocalGUCPgx(ctx, tx, tenant.FromContextOrDefault(ctx))
}

// setTenantLocalGUCPgx is setTenantLocalGUC for the pgx side: transaction-
// local set_config so the GUC reverts when the tx ends and can never leak
// onto a pooled connection.
func setTenantLocalGUCPgx(ctx context.Context, tx pgx.Tx, id tenant.ID) error {
	if id.IsZero() {
		return fmt.Errorf("db: cannot set %s GUC for a zero tenant id", TenantGUC)
	}
	var out string
	if err := tx.QueryRow(ctx,
		"SELECT set_config($1, $2, TRUE)", TenantGUC, id.String(),
	).Scan(&out); err != nil {
		return fmt.Errorf("db: set %s GUC: %w", TenantGUC, err)
	}
	return nil
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

// newSQLTracerFromEnv installs a query tracer on the pgx pool when
// OPENRAILS_SQL_TRACE is set (1/true/debug). Replaces the bundebug hook from
// the bun era (which was likewise off by default).
func newSQLTracerFromEnv() *tracelog.TraceLog {
	v := os.Getenv("OPENRAILS_SQL_TRACE")
	if v == "" || v == "0" || v == "false" {
		return nil
	}
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
