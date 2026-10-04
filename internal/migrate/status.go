package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/open-rails/migratekit"

	"github.com/open-rails/openrails/internal/config"
)

// PostgresStatus reports the embedded chain against the database's ledger:
// applied, pending, and every discrepancy, as migratekit reports them.
func PostgresStatus(ctx context.Context, cfg *config.Config) (status migratekit.Status, err error) {
	if cfg == nil || cfg.DB == nil {
		return status, fmt.Errorf("missing database config")
	}
	schema := config.SchemaName(cfg)
	migrations, err := loadMigrations(schema)
	if err != nil {
		return status, err
	}
	sqlDB, err := sql.Open("pgx", config.DBConnectionString(cfg.DB))
	if err != nil {
		return status, fmt.Errorf("open postgres: %w", err)
	}
	defer func() { err = errors.Join(err, sqlDB.Close()) }()
	return migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema).Status(ctx, migrations)
}
