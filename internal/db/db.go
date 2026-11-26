package db

import (
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the application's database handle: a pgx/v5 pool that all
// sqlc-generated queries run on (Qx/Gen). A DB can also be transaction-scoped
// (pgtx set, via NewWithPgxTx) so services can hand repos a tx-bound DB.
//
// Nothing it runs on the pool outlives its transaction: no session setting,
// lock or prepared statement, so the pool may be a transaction pooler
// (PgBouncer in transaction mode). What needs a session (LISTEN, session
// advisory locks, migrations) dials one: from direct when set, else from the
// pool's configuration.
type DB struct {
	pool  *pgxpool.Pool
	pgtx  pgx.Tx
	river *runtimeBinding
	// direct is the session configuration when pool is a transaction pooler,
	// copied from the host.
	direct *pgxpool.Config

	// ownsPool: Close closes the pool (NewOwningPGXPool, SeparatePool);
	// pools injected by embedded hosts (NewWithPGXPool) stay open.
	ownsPool bool

	// rw relocates statements to the configured schema (#471); identity for
	// the default schema. Inherited by tx-scoped wrappers.
	rw schemaRewriter

	Log *slog.Logger
}

// NewWithPGXPool wraps a host-supplied pgx pool (the embedded-host path). The
// host keeps ownership of its pool; Close() is a no-op for it. schema is the
// OpenRails Postgres schema (config.SchemaName); when it differs from
// the default, runtime queries are rewritten to it (#471) — essential here
// because OpenRails shares the host's pool and cannot repoint its search_path.
func NewWithPGXPool(logger *slog.Logger, pool *pgxpool.Pool, schema string) (*DB, error) {
	return NewWithPGXPoolAndSessions(logger, pool, schema, nil)
}

// NewOwningPGXPool wraps pool and takes it over: Close closes it. For a
// one-off process that opened its own pool.
func NewOwningPGXPool(logger *slog.Logger, pool *pgxpool.Pool, schema string) (*DB, error) {
	d, err := NewWithPGXPool(logger, pool, schema)
	if err != nil {
		return nil, err
	}
	d.ownsPool = true
	return d, nil
}

// NewWithPGXPoolAndSessions uses direct for session work when pool is a
// transaction pooler. The host still owns pool.
func NewWithPGXPoolAndSessions(logger *slog.Logger, pool *pgxpool.Pool, schema string, direct *pgxpool.Config) (*DB, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is nil")
	}
	if direct != nil {
		if direct.ConnConfig == nil {
			return nil, fmt.Errorf("direct Postgres session configuration has no connection settings")
		}
		if err := RequirePoolerSafe(pool); err != nil {
			return nil, err
		}
		direct = direct.Copy()
	}
	return &DB{river: &runtimeBinding{}, pool: pool, rw: newSchemaRewriter(schema), direct: direct, Log: logger}, nil
}

// DataPool returns a schema-aware wrapper over the base pool for the rare
// hand-written queries that run directly on the pool (admin/CLI tools, the audit
// checker) instead of through the sqlc Querier. nil when this DB has no pool.
func (d *DB) DataPool() *Pool {
	if d == nil || d.pool == nil {
		return nil
	}
	return &Pool{raw: d.pool, rw: d.rw, schema: d.rw.schema(), river: d.river}
}

// Pool exposes the pgx pool (nil for tx-scoped wrappers).
func (d *DB) Pool() *pgxpool.Pool {
	if d == nil {
		return nil
	}
	return d.pool
}

func (d *DB) Close() error {
	if d.ownsPool && d.pool != nil {
		d.pool.Close()
	}
	return nil
}

// NewWithPgxTx returns a DB scoped to an open pgx transaction: Qx/Gen return
// the transaction, so repos called with this DB run inside it.
func NewWithPgxTx(logger *slog.Logger, tx pgx.Tx) *DB {
	if scoped, ok := tx.(schemaTx); ok {
		return &DB{pgtx: tx, rw: scoped.rw, river: scoped.river, Log: logger}
	}
	rw := newSchemaRewriter("")
	return &DB{pgtx: rw.wrapTx(tx), rw: rw, river: &runtimeBinding{}, Log: logger}
}

func (d *DB) NewWithPgxTx(tx pgx.Tx) *DB {
	return &DB{pgtx: schemaTx{Tx: tx, rw: d.rw, river: d.river}, rw: d.rw, river: d.river, Log: d.Log}
}

// RequirePoolerSafe refuses a pool whose statements need a server-side
// prepared statement to outlive one round trip: a transaction pooler may run
// the next round trip on another server connection.
func RequirePoolerSafe(pool *pgxpool.Pool) error {
	switch mode := pool.Config().ConnConfig.DefaultQueryExecMode; mode {
	case pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol:
		return nil
	default:
		return fmt.Errorf("a transaction-pooled Postgres pool (one with a direct session configuration) needs DefaultQueryExecMode cache_describe, exec or simple_protocol, not %s", mode)
	}
}
