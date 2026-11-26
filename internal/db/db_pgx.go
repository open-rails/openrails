package db

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
)

// MerchantGUC carries the selected merchant for stored functions and SQL that
// calls current_merchant_id(). It filters nothing else: tenant isolation is
// the explicit merchant predicate and composite keys.
const MerchantGUC = "openrails.merchant_id"

// Qx returns the handle sqlc queries use: the open transaction this DB is
// scoped to (NewWithPgxTx), else the pool, whose statements run as the
// context's merchant scope (WithMerchantConn). Choosing a handle authorizes
// nothing; tenant queries still carry their verified merchant predicate.
func (d *DB) Qx(ctx context.Context) gen.DBTX {
	if d == nil {
		return errDBTX{fmt.Errorf("db: Qx on nil DB")}
	}
	// d.pgtx already rewrites schemas (pgxBegin / Pool.Begin); the pool is
	// raw, so wrap it here.
	if d.pgtx != nil {
		return d.pgtx
	}
	if _, err := d.merchantScope(ctx); err != nil {
		return errDBTX{err}
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
	if _, err := d.merchantScope(ctx); err != nil {
		return nil, err
	}
	if d.pool != nil {
		tx, err := pooledDBTX{pool: d.pool, directory: true}.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return schemaTx{Tx: tx, rw: d.rw, river: d.river}, nil
	}
	return nil, fmt.Errorf("db: no pgx handle available to begin transaction")
}

// RunInTx runs fn in a transaction for work that uses explicit merchant_id
// predicates. Within a merchant scope it sets the merchant locally.
func (d *DB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := d.pgxBegin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := d.bindScopedMerchant(ctx, tx); err != nil {
		return err
	}
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

// bindScopedMerchant sets the context's merchant scope locally in tx.
// Snapshot callers must set their transaction mode before this first query.
func (d *DB) bindScopedMerchant(ctx context.Context, tx pgx.Tx) error {
	scope, err := d.merchantScope(ctx)
	if err != nil || scope == nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return setMerchantLocalGUCPgx(ctx, tx, mid)
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

// NewSQLTracer logs every query at debug level, for a pool a host opens
// with sql_trace on. It logs each statement, never its arguments: they carry
// customer data.
func NewSQLTracer(logger *slog.Logger) *tracelog.TraceLog {
	return &tracelog.TraceLog{
		Logger: tracelog.LoggerFunc(func(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
			logPGX(ctx, logger, level, msg, data)
		}),
		LogLevel: tracelog.LogLevelDebug,
	}
}

func logPGX(ctx context.Context, logger *slog.Logger, level tracelog.LogLevel, msg string, data map[string]any) {
	fields := []any{}
	for _, k := range slices.Sorted(maps.Keys(data)) {
		switch v := data[k].(type) {
		case error:
			fields = append(fields, logx.Err(v))
		default:
			if k != "args" {
				fields = append(fields, k, v)
			}
		}
	}
	entry := logger.With(fields...)
	switch level {
	case tracelog.LogLevelTrace, tracelog.LogLevelDebug:
		entry.DebugContext(ctx, msg)
	case tracelog.LogLevelInfo:
		entry.InfoContext(ctx, msg)
	case tracelog.LogLevelWarn:
		entry.WarnContext(ctx, msg)
	default:
		entry.ErrorContext(ctx, msg)
	}
}
