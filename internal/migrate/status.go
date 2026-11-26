package migrate

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"

	"github.com/open-rails/openrails/internal/config"
)

// PostgresStatus reports the embedded chain against the database's ledger:
// applied, pending, and every discrepancy, as migratekit reports them; an
// edited migration is an error, as Apply refuses it.
func PostgresStatus(ctx context.Context, pool *pgxpool.Pool, schema string) (status migratekit.Status, err error) {
	if pool == nil {
		return status, errors.New("migration status needs a pool")
	}
	migrations, err := loadMigrations(schema)
	if err != nil {
		return status, err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { err = errors.Join(err, sqlDB.Close()) }()
	return migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema).WithImmutableMigrations().Status(ctx, migrations)
}
