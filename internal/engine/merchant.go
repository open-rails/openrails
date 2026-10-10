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

// upsertMerchantConfig provisions the constructor's merchant declaration before
// HTTP routes or workers can observe a partially configured runtime. With
// tolerateVault, a Solana vault_transit signer whose Vault is unavailable
// reuses the identity stored for it, or (none stored yet) leaves only that PSP
// unprovisioned; the returned flag says Vault must still confirm it.
func upsertMerchantConfig(ctx context.Context, a *app.App, slug string, m config.MerchantDeclaration, tolerateVault bool) (billing.MerchantID, bool, error) {
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
	bound := a.Runtime.ConfiguredMerchant()
	if !bound.IsZero() {
		selected, err := directory.GetBySlug(ctx, billing.NormalizeMerchantSlug(slug))
		if err != nil {
			return billing.MerchantID{}, false, fmt.Errorf("openrails: engine is already bound to merchant %s; cannot resolve supplied name %q: %w", bound, slug, err)
		}
		if selected.ID != bound {
			return billing.MerchantID{}, false, fmt.Errorf("openrails: one embedded engine serves one merchant; refusing second merchant %q (%s), bound to %s", slug, selected.ID, bound)
		}
		slug = selected.Slug
	}

	// The same provisioning openrails.New and the server use: with a file the
	// declaration is the configuration; with Vault it seeds Vault once.
	req := boot.ProvisionMerchantParams{
		Config:     conf,
		Database:   database,
		MerchantID: bound,
		Merchants:  directory,
		Slug:       slug,
		Merchant:   m,
		Insert:     true,
	}
	var fallback *signeridentity.Transit
	if a.Runtime.Vault != nil && a.Runtime.Vault.SolanaTransit != nil {
		fallback = &signeridentity.Transit{TransitClient: a.Runtime.Vault.SolanaTransit, DB: database, Directory: directory, Slug: slug,
			Environment: config.ExpectedProviderEnvironment(config.IsTestMode(conf)), Tolerate: tolerateVault, OnChange: a.Runtime.ReportSignerKeyChange}
		req.SolanaTransit = fallback
		req.DeferPSP = fallback.Defers
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return billing.MerchantID{}, false, fmt.Errorf("openrails: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant().IsZero() {
		a.Runtime.SetConfiguredMerchant(tn.ID)
	}
	return tn.ID, fallback != nil && fallback.Unavailable(), nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the declaration, provisioning it.
func approveSolanaSigner(ctx context.Context, a *app.App, declaration config.MerchantDeclaration, mid billing.MerchantID, key string) error {
	if declaration.Slug == "" {
		return fmt.Errorf("openrails: no merchant declaration to approve a signer for")
	}
	if a.Runtime.Vault == nil || a.Runtime.Vault.SolanaTransit == nil || a.Runtime.Merchants == nil {
		return fmt.Errorf("openrails: no Vault Transit signer is configured")
	}
	environment := config.ExpectedProviderEnvironment(config.IsTestMode(a.Config))
	if _, err := signeridentity.Approve(ctx, a.Runtime.DB, a.Runtime.Merchants, a.Runtime.Vault.SolanaTransit, mid, environment, key); err != nil {
		return fmt.Errorf("openrails: %w", err)
	}
	if _, _, err := upsertMerchantConfig(ctx, a, declaration.Slug, declaration, false); err != nil {
		return err
	}
	a.Runtime.ReportSignerKeyChange(nil)
	return nil
}

// configureMerchant reconciles the declared merchant before routes or workers
// can observe it. It reports whether a Solana Transit signer still awaits
// Vault (see upsertMerchantConfig); confirmSigner completes it.
func configureMerchant(ctx context.Context, application *app.App, declaration config.MerchantDeclaration) (bool, error) {
	if declaration.Slug == "" {
		return false, nil
	}
	_, pending, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration, true)
	return pending, err
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
			_, _, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration, false)
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
