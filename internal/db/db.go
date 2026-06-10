package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/sirupsen/logrus"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// DB wraps the application's database handles. During the bun -> sqlc
// transition (issue #334) it carries both sides over SEPARATE pools:
//
//   - pool: the pgx/v5 pool that sqlc-generated queries run on (Qx/Gen).
//   - db:   the bun handle on its OWN database/sql pool (pgx stdlib driver).
//     Removed in Phase 2.
//
// The pools MUST be separate: serving both sides from one pool means a
// request that holds a bun connection and then waits for a pgx connection
// can deadlock the whole pool under burst (hold-and-wait on a single
// resource class — the Phase-0 twin-pinning wedge, see issue #334 status).
// With separate pools, requests queue on the first pool BEFORE holding
// anything, so cross-pool starvation cannot occur. The transitional cost is
// a higher combined connection ceiling (2x30), removed with the bun side.
//
// A DB can also be transaction-scoped (one of pgtx / bun.Tx set) so services
// can hand repos a tx-bound DB.
type DB struct {
	db   bun.IDB
	pool *pgxpool.Pool
	pgtx pgx.Tx

	// ownsPool: NewDB created the pool and Close() must close it; pools
	// injected by embedded hosts (NewWithPGXPool) stay open.
	ownsPool bool
}

const (
	dbConnectMaxWait     = 60 * time.Second
	dbConnectBaseDelay   = time.Second
	dbConnectMaxDelay    = 5 * time.Second
	dbConnectPingTimeout = 5 * time.Second

	dbMaxOpenConns    = 30
	dbMaxIdleConns    = 10
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

	pool, err := newTunedPGXPool(context.Background(), url)
	if err != nil {
		return nil, err
	}

	db, sqldb, err := newBunSideDB(url)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := pingWithRetry(context.Background(), func(ctx context.Context) error {
		return db.PingContext(ctx)
	}, "database"); err != nil {
		if cerr := sqldb.Close(); cerr != nil {
			logrus.Errorf("failed to close database connection: %v", cerr)
		}
		pool.Close()
		return nil, err
	}

	return &DB{db: db, pool: pool, ownsPool: true}, nil
}

// newBunSideDB builds the transition-era bun handle on its OWN
// database/sql pool (pgx stdlib driver) — deliberately NOT sharing the pgx
// pool (see the DB doc comment on the twin-pinning deadlock).
func newBunSideDB(url string) (*bun.DB, *sql.DB, error) {
	// pgxpool.ParseConfig (not pgx.ParseConfig) so pool_* DSN params an
	// operator set for the pgx side are tolerated rather than fatal.
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, nil, fmt.Errorf("parse postgres config: %w", err)
	}
	sqldb := stdlib.OpenDB(*pcfg.ConnConfig)
	applySQLPoolTuning(sqldb)
	db := bun.NewDB(sqldb, pgdialect.New())
	models.RegisterModels(db)
	return db, sqldb, nil
}

// newTunedPGXPool parses the connection string, applies the pool tuning that
// applySQLPoolTuning historically applied to the database/sql pool, installs
// the optional query tracer, and verifies connectivity with retry.
func newTunedPGXPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	pcfg.MaxConns = dbMaxOpenConns
	pcfg.MaxConnLifetime = dbConnMaxLifetime
	pcfg.MaxConnIdleTime = dbConnMaxIdleTime
	if tracer := newSQLTracerFromEnv(); tracer != nil {
		pcfg.ConnConfig.Tracer = tracer
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

func applySQLPoolTuning(sqldb *sql.DB) {
	sqldb.SetMaxOpenConns(dbMaxOpenConns)
	sqldb.SetMaxIdleConns(dbMaxIdleConns)
	sqldb.SetConnMaxLifetime(dbConnMaxLifetime)
	sqldb.SetConnMaxIdleTime(dbConnMaxIdleTime)

	logrus.WithFields(logrus.Fields{
		"db_max_open_conns":     dbMaxOpenConns,
		"db_max_idle_conns":     dbMaxIdleConns,
		"db_conn_max_lifetime":  dbConnMaxLifetime.String(),
		"db_conn_max_idle_time": dbConnMaxIdleTime.String(),
	}).Info("Applied SQL connection pool settings")
}

// NewWithSQLDB wraps a host-supplied *sql.DB. Transition note (#334): this
// path has NO pgx pool, so sqlc-converted code paths are unavailable on it —
// embedded hosts should supply a pgx pool (embedded.Options.PGXPool) instead.
// Scheduled for removal in Phase 2 of #334 (no known production user).
func NewWithSQLDB(sqlDB *sql.DB) (*DB, error) {
	if sqlDB == nil {
		return nil, fmt.Errorf("sql db is nil")
	}
	db := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(db)
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return &DB{db: db}, nil
}

// NewWithBun wraps a host-supplied *bun.DB. Same transition caveat as
// NewWithSQLDB: no pgx pool, sqlc paths unavailable, removed in Phase 2.
func NewWithBun(bunDB *bun.DB) (*DB, error) {
	if bunDB == nil {
		return nil, fmt.Errorf("bun db is nil")
	}
	models.RegisterModels(bunDB)
	if err := bunDB.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return &DB{db: bunDB}, nil
}

// NewWithPGXPool wraps a host-supplied pgx pool (the embedded-host path).
// The host pool serves the sqlc side; the transitional bun side gets its own
// database/sql pool built from the same connection config (separate pools —
// see the DB doc comment on the twin-pinning deadlock). The host keeps
// ownership of its pool; Close() closes only the bun-side pool.
func NewWithPGXPool(pool *pgxpool.Pool) (*DB, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is nil")
	}
	connCfg := pool.Config().ConnConfig.Copy()
	sqldb := stdlib.OpenDB(*connCfg)
	applySQLPoolTuning(sqldb)
	db := bun.NewDB(sqldb, pgdialect.New())
	models.RegisterModels(db)
	if err := db.PingContext(context.Background()); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return &DB{db: db, pool: pool}, nil
}

func (d *DB) GetDB() bun.IDB {
	return d.db
}

// Pool exposes the pgx pool (nil for tx-scoped or bun-only wrappers).
func (d *DB) Pool() *pgxpool.Pool {
	if d == nil {
		return nil
	}
	return d.pool
}

func (d *DB) QualifiedTable(tableName string) string {
	return fmt.Sprintf("billing.%s", tableName)
}

func (d *DB) Close() error {
	var err error
	if bunDB, ok := d.db.(*bun.DB); ok {
		if underlyingDB := bunDB.DB; underlyingDB != nil {
			err = underlyingDB.Close()
		}
	}
	if d.ownsPool && d.pool != nil {
		d.pool.Close()
	}
	return err
}

// NewWithTx returns a DB scoped to an open bun transaction (transition-era;
// converted code uses NewWithPgxTx).
func NewWithTx(tx bun.Tx) *DB {
	return &DB{
		db: tx,
	}
}

func (d *DB) NewWithTx(tx bun.Tx) *DB {
	return &DB{
		db: tx,
	}
}

// NewWithPgxTx returns a DB scoped to an open pgx transaction: Qx/Gen return
// the transaction, so repos called with this DB run inside it. The pgx
// analogue of NewWithTx.
func NewWithPgxTx(tx pgx.Tx) *DB {
	return &DB{pgtx: tx}
}

func (d *DB) NewWithPgxTx(tx pgx.Tx) *DB {
	return &DB{pgtx: tx}
}
