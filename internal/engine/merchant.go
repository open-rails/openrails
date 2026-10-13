package engine

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/signeridentity"
)

// upsertMerchantConfig provisions one declared merchant before HTTP routes or
// workers can observe a partially configured runtime. With tolerateVault, a
// Solana vault_transit signer whose Vault is unavailable reuses the identity
// stored for it, or (none stored yet) leaves only that PSP unprovisioned; the
// returned flag says Vault must still confirm it.
func upsertMerchantConfig(ctx context.Context, a *app.App, m config.MerchantDeclaration, tolerateVault bool) (billing.MerchantID, bool, error) {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return billing.MerchantID{}, false, fmt.Errorf("openrails: app database not initialized")
	}
	conf := a.Config
	if conf == nil || conf.DB == nil {
		return billing.MerchantID{}, false, fmt.Errorf("openrails: config/db is required")
	}
	database := a.Runtime.DB

	directory := a.Runtime.Merchants
	if directory == nil {
		return billing.MerchantID{}, false, fmt.Errorf("openrails: merchant configuration is not wired")
	}

	// The same provisioning openrails.New and the server use: with a file the
	// declaration is the configuration; with Vault it seeds Vault once.
	req := boot.ProvisionMerchantParams{
		Config:    conf,
		Database:  database,
		Merchants: directory,
		Slug:      m.Slug,
		Merchant:  m,
		Insert:    true,
	}
	var fallback *signeridentity.Transit
	if a.Runtime.Vault != nil && a.Runtime.Vault.SolanaTransit != nil {
		fallback = &signeridentity.Transit{TransitClient: a.Runtime.Vault.SolanaTransit, DB: database, Directory: directory, Slug: m.Slug,
			Tolerate: tolerateVault, OnChange: a.Runtime.ReportSignerKeyChange}
		req.SolanaTransit = fallback
		req.DeferPSP = fallback.Defers
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return billing.MerchantID{}, false, fmt.Errorf("openrails: upsert merchant config: %w", err)
	}
	return tn.ID, fallback != nil && fallback.Unavailable(), nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the declaration, provisioning it.
func approveSolanaSigner(ctx context.Context, a *app.App, declaration config.MerchantDeclaration, mid billing.MerchantID, key string) error {
	if declaration.Slug == "" {
		return fmt.Errorf("openrails: Config declares no merchant %s to approve a signer for", mid)
	}
	if a.Runtime.Vault == nil || a.Runtime.Vault.SolanaTransit == nil || a.Runtime.Merchants == nil {
		return fmt.Errorf("openrails: no Vault Transit signer is configured")
	}
	if _, err := signeridentity.Approve(ctx, a.Runtime.DB, a.Runtime.Merchants, a.Runtime.Vault.SolanaTransit, mid, key); err != nil {
		return fmt.Errorf("openrails: %w", err)
	}
	if _, _, err := upsertMerchantConfig(ctx, a, declaration, false); err != nil {
		return err
	}
	a.Runtime.ReportSignerKeyChange(nil)
	return nil
}

// confirmSigner re-runs the declaration once Vault authenticates, arming a
// deferred Solana Transit PSP or re-deriving a stored identity from Vault,
// with capped full-jitter backoff until it succeeds.
func confirmSigner(application *app.App, declaration config.MerchantDeclaration) {
	rt := application.Runtime
	rt.Go("solana transit signer", func(ctx context.Context) {
		err := retry.Forever(ctx, func(ctx context.Context) error {
			if err := rt.Vault.State(); err != nil {
				return err
			}
			_, _, err := upsertMerchantConfig(ctx, application, declaration, false)
			return err
		}, func(attempt int, err error) {
			if attempt == 0 {
				log.WithError(err).Warn("openrails: Solana Transit signer awaits Vault; retrying in the background")
			}
		})
		if err == nil {
			log.Info("openrails: Solana Transit signer confirmed by Vault")
		}
	})
}
