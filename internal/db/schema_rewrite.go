package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// Schema relocation (#471, #1123). OpenRails SQL is authored in
// config.DefaultSchema and runs verbatim there. Embedded OpenRails shares the
// host's pool and cannot repoint its search_path, so any other configured schema
// is reached by rewriting each statement with sqlschema just before execution.

// schemaRewriter relocates statements to one schema. The zero value is the
// default schema (identity).
type schemaRewriter struct {
	rel *sqlschema.Relocator
}

func newSchemaRewriter(schema string) schemaRewriter {
	return schemaRewriter{rel: sqlschema.New(schema)}
}

func (r schemaRewriter) apply(sql string) (string, error) { return r.rel.SQL(sql) }

// schema is the schema this rewriter targets.
func (r schemaRewriter) schema() string { return r.rel.Schema() }

// ---- sqlc DBTX wrapper (covers every gen.Queries call site via DB.Qx) ----

// schemaDBTX wraps a gen.DBTX (pool / merchant connection / tx) and relocates
// every statement before it is executed.
type schemaDBTX struct {
	inner gen.DBTX
	rw    schemaRewriter
}

func (s schemaDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	sql, err := s.rw.apply(sql)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return s.inner.Exec(ctx, sql, args...)
}

func (s schemaDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	sql, err := s.rw.apply(sql)
	if err != nil {
		return nil, err
	}
	return s.inner.Query(ctx, sql, args...)
}

func (s schemaDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	sql, err := s.rw.apply(sql)
	if err != nil {
		return errRow{err}
	}
	return s.inner.QueryRow(ctx, sql, args...)
}

// wrapDBTX returns inner unchanged for the default schema.
func (r schemaRewriter) wrapDBTX(inner gen.DBTX) gen.DBTX {
	if r.rel == nil || inner == nil {
		return inner
	}
	return schemaDBTX{inner: inner, rw: r}
}

// ---- pgx.Tx wrapper (covers raw queries inside RunInTx / MerchantTx / Pool.Begin) ----

// schemaTx wraps a pgx.Tx so hand-written SQL executed on it is relocated.
// It embeds the underlying Tx, so Commit/Rollback/CopyFrom/SendBatch/LargeObjects/
// Conn delegate unchanged; only the SQL-carrying methods are intercepted.
type schemaTx struct {
	pgx.Tx
	rw    schemaRewriter
	river *runtimeBinding
}

func (t schemaTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return schemaDBTX{t.Tx, t.rw}.Exec(ctx, sql, args...)
}

func (t schemaTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return schemaDBTX{t.Tx, t.rw}.Query(ctx, sql, args...)
}

func (t schemaTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return schemaDBTX{t.Tx, t.rw}.QueryRow(ctx, sql, args...)
}

func (t schemaTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	sql, err := t.rw.apply(sql)
	if err != nil {
		return nil, err
	}
	return t.Tx.Prepare(ctx, name, sql)
}

func (t schemaTx) Begin(ctx context.Context) (pgx.Tx, error) {
	inner, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return schemaTx{Tx: inner, rw: t.rw, river: t.river}, nil
}

// wrapTx retains the configured schema even when it is the default and needs
// no SQL rewrite. NewWithPgxTx must distinguish that transaction from a raw host
// transaction, whose schema defaults to billing. Commit/Rollback still delegate.
func (r schemaRewriter) wrapTx(tx pgx.Tx) pgx.Tx {
	if tx == nil {
		return tx
	}
	if existing, ok := tx.(schemaTx); ok {
		return schemaTx{Tx: tx, rw: r, river: existing.river}
	}
	return schemaTx{Tx: tx, rw: r}
}

// ---- pgx pool wrapper (covers hand-written queries on the control-plane pool) ----

// Pool wraps a *pgxpool.Pool and relocates the SQL of hand-written queries
// run directly against it (the control-plane / tenancy / platform code paths,
// which don't go through the sqlc Querier). It mirrors the subset of
// *pgxpool.Pool that those call sites use; reach for Raw() when a raw pool is
// required (River, AuthKit, health pings).
type Pool struct {
	raw    *pgxpool.Pool
	rw     schemaRewriter
	schema string
	river  *runtimeBinding
}

// WrapPool wraps a raw pool for the configured schema; it is a pass-through for
// the default schema. Returns nil when raw is nil so `pool == nil` guards at
// call sites keep working.
func WrapPool(raw *pgxpool.Pool, schema string) *Pool {
	if raw == nil {
		return nil
	}
	if schema == "" {
		schema = config.DefaultSchema
	}
	return &Pool{raw: raw, rw: newSchemaRewriter(schema), schema: schema}
}

// Raw returns the underlying pool for APIs that need it verbatim (River, AuthKit,
// connectivity checks). SQL run on the raw pool is NOT relocated.
func (p *Pool) Raw() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.raw
}

// Schema returns the configured OpenRails schema of this pool.
func (p *Pool) Schema() string {
	if p == nil || p.schema == "" {
		return config.DefaultSchema
	}
	return p.schema
}

// Pool statements reuse the request's pinned connection when it is idle, and
// otherwise wait a bounded time for a pooled one (#1105).
func (p *Pool) handle() pooledDBTX { return pooledDBTX{pool: p.raw, schema: p.rw.schema()} }

func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return schemaDBTX{p.handle(), p.rw}.Exec(ctx, sql, args...)
}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return schemaDBTX{p.handle(), p.rw}.Query(ctx, sql, args...)
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return schemaDBTX{p.handle(), p.rw}.QueryRow(ctx, sql, args...)
}

func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.handle().Begin(ctx)
	if err != nil {
		return nil, err
	}
	return schemaTx{Tx: tx, rw: p.rw, river: p.river}, nil
}

// MerchantTx runs hand-written pool queries inside a transaction whose
// openrails.merchant_id GUC is pinned to the target merchant. Use this for direct Pool
// stores that touch merchant-owned tables but do not have a *DB.
func (p *Pool) MerchantTx(ctx context.Context, id billing.MerchantID, fn func(context.Context, pgx.Tx) error) error {
	if p == nil {
		return errors.New("db: MerchantTx on nil Pool")
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setMerchantLocalGUCPgx(ctx, tx, id); err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, id)
	if err := fn(transactionContext(ctx), tx); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (p *Pool) Ping(ctx context.Context) error { return p.raw.Ping(ctx) }

func (p *Pool) Stat() *pgxpool.Stat { return p.raw.Stat() }

func (p *Pool) Close() { p.raw.Close() }

// RewriteDBTX applies the same relocation as DB.Qx to a raw pool,
// connection, or transaction. Use it when constructing sqlc queries without DB.
func RewriteDBTX(inner gen.DBTX, schema string) gen.DBTX {
	return newSchemaRewriter(schema).wrapDBTX(inner)
}
