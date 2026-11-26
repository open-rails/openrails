package guide

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
)

// EmbeddedPgBouncer builds the engine behind PgBouncer in transaction mode.
func EmbeddedPgBouncer(ctx context.Context, cfg openrails.Config) (*openrails.Engine, error) {
	// guide:embedded-pgbouncer
	pooled, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL")) // PgBouncer, transaction mode
	if err != nil {
		return nil, err
	}
	pooled.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pool, err := pgxpool.NewWithConfig(ctx, pooled)
	if err != nil {
		return nil, err
	}
	session, err := pgxpool.ParseConfig(os.Getenv("DATABASE_SESSION_URL")) // direct
	if err != nil {
		return nil, err
	}
	bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool, PostgresSession: session})
	// guide:end
	return bill, err
}
