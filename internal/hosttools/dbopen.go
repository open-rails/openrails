package hosttools

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
)

// openEmbeddedDB borrows the host pool or opens the configured database.
// Callers provide explicit merchant scope; Close leaves borrowed pools open.
func openEmbeddedDB(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var (
		database *db.DB
		err      error
	)
	if pool != nil {
		schema := config.SchemaName(cfg)
		database, err = db.NewWithPGXPool(pool, schema)
		if err != nil {
			return nil, err
		}
	} else {
		if cfg == nil || cfg.DB == nil {
			return nil, fmt.Errorf("config database is required")
		}
		database, err = db.NewDB(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
	}
	return database, nil
}

// bindMerchantConfig binds the configuration database reads merchant settings
// through: given (a running app's), else one this process opens.
func bindMerchantConfig(ctx context.Context, cfg *config.Config, database *db.DB, merchantID billing.MerchantID, given db.MerchantConfig) (func(), error) {
	if given != nil {
		database.SetMerchantConfig(given)
		return func() {}, nil
	}
	_, closeConfig, err := boot.OneOffMerchants(ctx, cfg, database, merchantID, nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("merchant configuration unavailable: %w", err)
	}
	return closeConfig, nil
}
