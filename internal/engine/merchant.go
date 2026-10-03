package engine

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// upsertMerchantConfig reconciles the constructor's merchant declaration before
// HTTP routes or workers can observe a partially configured runtime. With
// tolerateVault, a Solana vault_transit signer whose Vault is unavailable
// reuses the identity stored for it, or (none stored yet) leaves only that PSP
// unprovisioned; the returned flag says Vault must still confirm it.
func upsertMerchantConfig(ctx context.Context, a *app.App, slug string, m config.MerchantDeclaration, tolerateVault bool) (merchant.ID, bool, error) {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails: app database not initialized")
	}
	conf := a.Config
	if conf == nil || conf.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails: config/db is required")
	}
	database := a.Runtime.DB

	directory := a.Runtime.Merchants
	if directory == nil {
		var err error
		directory, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return merchant.ID{}, false, err
		}
	}
	bound := a.Runtime.ConfiguredMerchant()
	if !bound.IsZero() {
		selected, err := directory.GetBySlug(ctx, merchant.NormalizeSlug(slug))
		if err != nil {
			return merchant.ID{}, false, fmt.Errorf("openrails: engine is already bound to merchant %s; cannot resolve supplied name %q: %w", bound, slug, err)
		}
		if selected.ID != bound {
			return merchant.ID{}, false, fmt.Errorf("openrails: one embedded engine serves one merchant; refusing second merchant %q (%s), bound to %s", slug, selected.ID, bound)
		}
		slug = selected.Slug
	}

	// Run it through the same billing-only merchant-provisioning boundary openrails.New
	// and the bootstrap CLI use, with ControlPlane nil. Provider-account reconcile
	// needs a merchant secret store (built over the engine's own pool).
	req := boot.ProvisionMerchantRequest{
		Config:     conf,
		Database:   database,
		MerchantID: bound,
		Directory:  directory,
		Slug:       slug,
		Merchant:   m,
		Options:    boot.MerchantManifestReconcileOptions{Insert: true, StripeClients: a.Runtime.StripeClients},
	}
	var fallback *signeridentity.Transit
	switch {
	case conf.SecretStoreBackend() == config.SecretBackendSnapshot:
		// Load the host-owned snapshot into this process. Metadata initialization
		// is create-only; authorized edits and archived accounts survive restarts.
		if a.Runtime == nil || a.Runtime.ManifestSecrets == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails: snapshot credentials require the runtime snapshot plane")
		}
		req.SecretStore = a.Runtime.ManifestSecrets.Seeder()
		backend := a.Runtime.MerchantSecretBackend
		if backend == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails: credential backend must be initialized before provisioning")
		}
		req.SolanaTransit = backend.SolanaTransit
		if backend.SolanaTransit != nil {
			fallback = &signeridentity.Transit{TransitClient: backend.SolanaTransit, DB: database, Directory: directory, Slug: slug,
				Environment: config.ExpectedProviderEnvironment(conf.IsTestMode()), Tolerate: tolerateVault, OnChange: a.Runtime.ReportSignerKeyChange}
			req.SolanaTransit = fallback
			req.Options.DeferPSP = fallback.Defers
		}
	case len(m.PSPs) > 0 || len(m.Custodians) > 0:
		return merchant.ID{}, false, fmt.Errorf("openrails: provider credential declarations require a host snapshot; use the Client payment-provider publication operation for managed credentials")
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, false, fmt.Errorf("openrails: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant().IsZero() {
		a.Runtime.SetConfiguredMerchant(tn.ID)
	}
	return tn.ID, fallback != nil && fallback.Unavailable(), nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the declaration, provisioning it.
func approveSolanaSigner(ctx context.Context, a *app.App, declaration config.MerchantDeclaration, mid merchant.ID, key string) error {
	if declaration.Slug == "" {
		return fmt.Errorf("openrails: no merchant declaration to approve a signer for")
	}
	backend := a.Runtime.MerchantSecretBackend
	if backend == nil {
		return fmt.Errorf("openrails: no Vault Transit signer is configured")
	}
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return err
	}
	environment := config.ExpectedProviderEnvironment(a.Config.IsTestMode())
	if _, err := signeridentity.Approve(ctx, a.Runtime.DB, directory, backend.SolanaTransit, mid, environment, key); err != nil {
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
			if err := rt.MerchantSecretBackend.State(); err != nil {
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
