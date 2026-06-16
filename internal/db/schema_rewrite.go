package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Schema rewriting (#471).
//
// OpenRails' SQL — sqlc-generated queries, hand-written queries, and migration
// DDL — is authored schema-qualified to config.DefaultSchema ("openrails"). A
// host can relocate every OpenRails table to a different Postgres schema via
// db.schema / DB_SCHEMA. Because OpenRails runs embedded on a pool it SHARES with
// the host, it cannot repoint that pool's search_path (that would hijack the
// host's own queries), so relocation is done by rewriting the schema qualifier in
// the SQL text just before execution.
//
// The rewrite is a no-op whenever the configured schema equals the default, so
// the overwhelmingly common deployment runs the SQL verbatim with zero overhead.
// schemaRewriter is therefore safe to install unconditionally.

// schemaRewriter rewrites the canonical `openrails.` qualifier to the configured
// schema's qualifier. The zero value is inactive (identity).
type schemaRewriter struct {
	from   string // e.g. "openrails."
	to     string // e.g. "shop."
	active bool
}

// newSchemaRewriter builds a rewriter for the configured schema. It is inactive
// (identity) when the schema is empty or already the default, so the default
// deployment pays nothing.
func newSchemaRewriter(schema string) schemaRewriter {
	if schema == "" || schema == config.DefaultSchema {
		return schemaRewriter{}
	}
	return schemaRewriter{from: config.DefaultSchema + ".", to: schema + ".", active: true}
}

// apply rewrites a SQL string. Cheap and identity when inactive.
func (r schemaRewriter) apply(sql string) string {
	if !r.active {
		return sql
	}
	return strings.ReplaceAll(sql, r.from, r.to)
}

// ---- sqlc DBTX wrapper (covers every gen.Queries call site via DB.Qx) ----

// schemaDBTX wraps a gen.DBTX (pool / merchant connection / tx) and rewrites the
// schema qualifier of every statement before it is executed.
type schemaDBTX struct {
	inner gen.DBTX
	rw    schemaRewriter
}

func (s schemaDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return s.inner.Exec(ctx, s.rw.apply(sql), args...)
}

func (s schemaDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.inner.Query(ctx, s.rw.apply(sql), args...)
}

func (s schemaDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.inner.QueryRow(ctx, s.rw.apply(sql), args...)
}

// wrapDBTX returns inner unchanged when the rewriter is inactive, else a schema-
// rewriting DBTX wrapper.
func (r schemaRewriter) wrapDBTX(inner gen.DBTX) gen.DBTX {
	if !r.active || inner == nil {
		return inner
	}
	return schemaDBTX{inner: inner, rw: r}
}

// ---- pgx.Tx wrapper (covers raw queries inside RunInTx / MerchantTx / Pool.Begin) ----

// schemaTx wraps a pgx.Tx so hand-written SQL executed on it is schema-rewritten.
// It embeds the underlying Tx, so Commit/Rollback/CopyFrom/SendBatch/LargeObjects/
// Conn delegate unchanged; only the SQL-carrying methods are intercepted.
type schemaTx struct {
	pgx.Tx
	rw schemaRewriter
}

func (t schemaTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(ctx, t.rw.apply(sql), args...)
}

func (t schemaTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(ctx, t.rw.apply(sql), args...)
}

func (t schemaTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.Tx.QueryRow(ctx, t.rw.apply(sql), args...)
}

func (t schemaTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return t.Tx.Prepare(ctx, name, t.rw.apply(sql))
}

func (t schemaTx) Begin(ctx context.Context) (pgx.Tx, error) {
	inner, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return schemaTx{Tx: inner, rw: t.rw}, nil
}

// wrapTx returns tx unchanged when the rewriter is inactive, else a schema-
// rewriting wrapper. Callers keep committing/rolling back the wrapper (it
// delegates those to the underlying tx).
func (r schemaRewriter) wrapTx(tx pgx.Tx) pgx.Tx {
	if !r.active || tx == nil {
		return tx
	}
	return schemaTx{Tx: tx, rw: r}
}

// ---- pgx pool wrapper (covers hand-written queries on the control-plane pool) ----

// Pool wraps a *pgxpool.Pool and schema-rewrites the SQL of hand-written queries
// run directly against it (the control-plane / tenancy / platform code paths,
// which don't go through the sqlc Querier). It mirrors the subset of
// *pgxpool.Pool that those call sites use; reach for Raw() when a raw pool is
// required (River, AuthKit, health pings).
type Pool struct {
	raw    *pgxpool.Pool
	rw     schemaRewriter
	schema string
}

// WrapPool wraps a raw pool with schema rewriting for the configured schema. The
// wrapper is a transparent pass-through when the schema is the default. Returns
// nil when raw is nil so `pool == nil` guards at call sites keep working.
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
// connectivity checks). SQL run on the raw pool is NOT schema-rewritten.
func (p *Pool) Raw() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.raw
}

// Schema returns the configured OpenRails schema this pool rewrites to.
func (p *Pool) Schema() string {
	if p == nil || p.schema == "" {
		return config.DefaultSchema
	}
	return p.schema
}

func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.raw.Exec(ctx, p.rw.apply(sql), args...)
}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.raw.Query(ctx, p.rw.apply(sql), args...)
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.raw.QueryRow(ctx, p.rw.apply(sql), args...)
}

func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.raw.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return p.rw.wrapTx(tx), nil
}

// MerchantTx runs hand-written pool queries inside a transaction whose
// app.merchant_id GUC is pinned to the target merchant. Use this for direct Pool
// stores that touch RLS-protected merchant-owned tables but do not have a *DB.
func (p *Pool) MerchantTx(ctx context.Context, id merchant.ID, fn func(context.Context, pgx.Tx) error) error {
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
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Pool) Ping(ctx context.Context) error { return p.raw.Ping(ctx) }

func (p *Pool) Stat() *pgxpool.Stat { return p.raw.Stat() }

func (p *Pool) Close() { p.raw.Close() }
