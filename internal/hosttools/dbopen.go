package hosttools

import (
	"context"
	"fmt"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
)

// openEmbeddedDB wraps the host's pool; Close leaves it open. Callers
// provide explicit merchant scope.
func openEmbeddedDB(cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if pool == nil {
		return nil, fmt.Errorf("a database pool is required")
	}
	return db.NewWithPGXPool(pool, config.SchemaName(cfg))
}

// bindMerchantConfig binds the configuration database reads merchant settings
// through: given (a running app's), else one this process opens.
func bindMerchantConfig(ctx context.Context, cfg *config.Config, database *db.DB, vault *vaultapi.Client, merchantID billing.MerchantID, given db.MerchantConfig) (func(), error) {
	if given != nil {
		database.SetMerchantConfig(given)
		return func() {}, nil
	}
	opened, err := boot.OneOffMerchants(ctx, cfg, database, vault, merchantID, nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("merchant configuration unavailable: %w", err)
	}
	return opened.Close, nil
}
