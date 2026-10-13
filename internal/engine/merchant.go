package engine

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
)

// upsertMerchantConfig provisions one declared merchant before HTTP routes or
// workers can observe a partially configured runtime. A document it cannot
// serve is refused alone (the cache reports it).
func upsertMerchantConfig(ctx context.Context, a *app.App, m config.MerchantDeclaration) (billing.MerchantID, error) {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return billing.MerchantID{}, fmt.Errorf("openrails: app database not initialized")
	}
	conf := a.Config
	if conf == nil || conf.DB == nil {
		return billing.MerchantID{}, fmt.Errorf("openrails: config/db is required")
	}
	directory := a.Runtime.Merchants
	if directory == nil {
		return billing.MerchantID{}, fmt.Errorf("openrails: merchant configuration is not wired")
	}
	// The same provisioning openrails.New and the server use: with a file the
	// declaration is the configuration; with Vault it seeds Vault once.
	tn, _, err := boot.ProvisionMerchant(ctx, boot.ProvisionMerchantParams{
		Config:    conf,
		Database:  a.Runtime.DB,
		Merchants: directory,
		Slug:      m.Slug,
		Merchant:  m,
		Insert:    true,
	})
	if err != nil {
		return billing.MerchantID{}, fmt.Errorf("openrails: upsert merchant config: %w", err)
	}
	return tn.ID, nil
}
