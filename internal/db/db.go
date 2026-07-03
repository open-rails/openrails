package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/sirupsen/logrus"
)

// DB wraps the application's database handle: a pgx/v5 pool that all
// sqlc-generated queries run on (Qx/Gen). A DB can also be transaction-scoped
// (pgtx set, via NewWithPgxTx) so services can hand repos a tx-bound DB.
//
// The bun ORM side that coexisted here during the #334 transition is gone:
// one driver, one pool.
type DB struct {
	pool *pgxpool.Pool
	pgtx pgx.Tx

	// ownsPool: NewDB created the pool and Close() must close it; pools
	// injected by embedded hosts (NewWithPGXPool) stay open.
	ownsPool bool

	// rw rewrites the canonical `openrails.` schema qualifier to the configured
	// schema (#471). Inactive (identity) for the default schema, so the common
	// path is a no-op. Inherited by tx-scoped wrappers.
	rw schemaRewriter
}

const (
	dbConnectMaxWait     = 60 * time.Second
	dbConnectBaseDelay   = time.Second
	dbConnectMaxDelay    = 5 * time.Second
	dbConnectPingTimeout = 5 * time.Second

	dbMaxOpenConns    = 30
	dbConnMaxLifetime = time.Hour
	dbConnMaxIdleTime = 15 * time.Minute
)

func NewDB(cfg *config.DBConfig) (_ *DB, err error) {
	if cfg == nil {
		return nil, fmt.Errorf("database config is nil")
	}
	url := cfg.GetConnectionString()
	if url == "" {
		return nil, fmt.Errorf("missing database configuration (DB_URL or DB_HOST/DB_PORT/etc.)")
	}

	pool, err := newTunedPGXPool(context.Background(), url, cfg.SQLTrace)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool, ownsPool: true, rw: newSchemaRewriter(cfg.SchemaName())}, nil
}

// newTunedPGXPool parses the connection string, applies the pool tuning,
// installs the optional query tracer, and verifies connectivity with retry.
func newTunedPGXPool(ctx context.Context, url string, sqlTrace bool) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	pcfg.MaxConns = dbMaxOpenConns
	pcfg.MaxConnLifetime = dbConnMaxLifetime
	pcfg.MaxConnIdleTime = dbConnMaxIdleTime
	if sqlTrace {
		pcfg.ConnConfig.Tracer = newSQLTracer()
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	if err := pingWithRetry(ctx, pool.Ping, "database"); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// NewPGXPoolWithRetry creates a pgx pool and eagerly verifies connectivity.
func NewPGXPoolWithRetry(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	if connString == "" {
		return nil, fmt.Errorf("missing postgres connection string")
	}
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	if err := pingWithRetry(ctx, pool.Ping, "pgx pool"); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func pingWithRetry(ctx context.Context, ping func(context.Context) error, label string) error {
	deadline := time.Now().Add(dbConnectMaxWait)
	delay := dbConnectBaseDelay
	var lastErr error
	for {
		pingCtx, cancel := context.WithTimeout(ctx, dbConnectPingTimeout)
		lastErr = ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("failed to connect to %s: %w", label, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("failed to connect to %s after %s: %w", label, dbConnectMaxWait, lastErr)
		}

		logrus.WithError(lastErr).Warnf("%s ping failed; retrying in %s", label, delay.Round(100*time.Millisecond))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("failed to connect to %s: %w", label, ctx.Err())
		case <-timer.C:
		}
		if delay < dbConnectMaxDelay {
			delay *= 2
			if delay > dbConnectMaxDelay {
				delay = dbConnectMaxDelay
			}
		}
	}
}

// NewWithPGXPool wraps a host-supplied pgx pool (the embedded-host path). The
// host keeps ownership of its pool; Close() is a no-op for it. schema is the
// OpenRails Postgres schema (config.DBConfig.SchemaName()); when it differs from
// the default, runtime queries are rewritten to it (#471) — essential here
// because OpenRails shares the host's pool and cannot repoint its search_path.
func NewWithPGXPool(pool *pgxpool.Pool, schema string) (*DB, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is nil")
	}
	return &DB{pool: pool, rw: newSchemaRewriter(schema)}, nil
}

// DataPool returns a schema-aware wrapper over the base pool for the rare
// hand-written queries that run directly on the pool (admin/CLI tools, the audit
// checker) instead of through the sqlc Querier. nil when this DB has no pool.
func (d *DB) DataPool() *Pool {
	if d == nil || d.pool == nil {
		return nil
	}
	return &Pool{raw: d.pool, rw: d.rw, schema: d.rw.schema()}
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
func NewWithPgxTx(tx pgx.Tx) *DB {
	return &DB{pgtx: tx}
}

func (d *DB) NewWithPgxTx(tx pgx.Tx) *DB {
	return &DB{pgtx: tx, rw: d.rw}
}
