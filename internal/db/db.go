package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/sirupsen/logrus"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bundebug"
)

type DB struct {
	db bun.IDB
}

func NewDB(cfg *config.DBConfig) (_ *DB, err error) {
	url := cfg.GetConnectionString()
	if url == "" {
		return nil, fmt.Errorf("missing database configuration (DB_URL or DB_HOST/DB_PORT/etc.)")
	}

	// Database is always PostgreSQL
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(url),
	))

	db := bun.NewDB(sqldb, pgdialect.New())
	models.RegisterModels(db)

	if err := db.PingContext(context.Background()); err != nil {
		if underlyingDB := db.DB; underlyingDB != nil {
			if cerr := underlyingDB.Close(); cerr != nil {
				logrus.Errorf("failed to close database connection: %v", cerr)
			}
		}

		return nil, fmt.Errorf("failed to connect to database: %w", err)
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
