package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/sirupsen/logrus"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bundebug"
)

type DB struct {
	db bun.IDB
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

	// Database is always PostgreSQL
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(url),
	))
	applySQLPoolTuning(sqldb)

	db := bun.NewDB(sqldb, pgdialect.New())
	models.RegisterModels(db)

	if err := pingWithRetry(context.Background(), func(ctx context.Context) error {
		return db.PingContext(ctx)
	}, "database"); err != nil {
		if underlyingDB := db.DB; underlyingDB != nil {
			if cerr := underlyingDB.Close(); cerr != nil {
				logrus.Errorf("failed to close database connection: %v", cerr)
			}
		}
		return nil, err
	}

	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(false),
		bundebug.WithEnabled(false),
	))

	dbInstance := &DB{
		db: db,
	}

	return dbInstance, nil
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

func NewWithSQLDB(sqlDB *sql.DB) (*DB, error) {
	if sqlDB == nil {
		return nil, fmt.Errorf("sql db is nil")
	}
	db := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(db)
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(false),
		bundebug.WithEnabled(false),
	))
	return &DB{db: db}, nil
}

func NewWithBun(bunDB *bun.DB) (*DB, error) {
	if bunDB == nil {
		return nil, fmt.Errorf("bun db is nil")
	}
	models.RegisterModels(bunDB)
	if err := bunDB.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	bunDB.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(false),
		bundebug.WithEnabled(false),
	))
	return &DB{db: bunDB}, nil
}

func NewWithPGXPool(pool *pgxpool.Pool) (*DB, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is nil")
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	db := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(db)
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(false),
		bundebug.WithEnabled(false),
	))
	return &DB{db: db}, nil
}

func (d *DB) GetDB() bun.IDB {
	return d.db
}

func (d *DB) QualifiedTable(tableName string) string {
	return fmt.Sprintf("billing.%s", tableName)
}

func (d *DB) Close() error {
	if bunDB, ok := d.db.(*bun.DB); ok {
		if underlyingDB := bunDB.DB; underlyingDB != nil {
			return underlyingDB.Close()
		}
	}
	return nil
}

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
