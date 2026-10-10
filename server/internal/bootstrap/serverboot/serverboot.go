// Package serverboot converges the standalone server's boot merchant manifest
// (#723) onto an engine built by openrails.New.
package serverboot

import (
	"context"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/server/internal/bootstrap"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// ReconcileBootMerchantManifest implements the standalone rows of the #723
// boot matrix, shared by cmd/openrails run-server and run-worker (#847): every
// boot ensures missing identities and reloads host-owned snapshot credentials.
// Existing metadata and archive decisions survive restart. The conventional
// path is optional; an explicitly supplied path must exist.
//
// Only Postgres can block it. A Vault Transit signer uses the runtime's own
// client (logged in in the background): while Vault is down its PSP keeps the
// stored identity, or on a first boot is deferred and provisioned in the
// background once Vault answers; a changed Transit key fails closed until an
// operator approves it (see ApproveSolanaSigner). Posture is verified in the
// background; nmiProbeV5BaseURL is the test-only posture probe seam. overlays
// are files in the manifest's shape merged over it in order.
func ReconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlays []string, nmiProbeV5BaseURL string) error {
	rt := application.Runtime
	if rt == nil {
		return fmt.Errorf("merchant startup requires a runtime")
	}
	// The merchant credential plane (and its Transit client) is built first so
	// the manifest reconcile signs with it rather than a Vault login of its own.
	if err := rt.EnsureMerchantsService(ctx); err != nil {
		log.WithError(err).Warn("merchant credentials unavailable at startup; sandbox PSPs verify on first use")
	}
	slugs, pending, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, true)
	if err != nil {
		return err
	}
	rt.ApproveSolanaSigner = func(ctx context.Context, mid billing.MerchantID, key string) error {
		return approveSolanaSigner(ctx, cfg, application, cp, path, overlays, mid, key)
	}
	if pending {
		rt.Go("solana transit signer", func(ctx context.Context) {
			err := retry.Forever(ctx, func(ctx context.Context) error {
				if err := rt.MerchantSecretBackend.State(); err != nil {
					return err
				}
				_, _, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, false)
				return err
			}, func(attempt int, err error) {
				if attempt == 0 {
					log.WithError(err).Warn("merchant startup: Solana Transit signer awaits Vault; retrying in the background")
				}
			})
			if err == nil {
				log.Info("merchant startup: Solana Transit signer confirmed by Vault")
			}
		})
	}
	if rt.Merchants == nil {
		return nil
	}
	var declared []billing.MerchantID
	for _, slug := range slugs {
		if m, err := rt.Merchants.GetBySlug(ctx, slug); err == nil {
			declared = append(declared, m.ID)
		}
	}
	rt.NMIPostureV5BaseURL = nmiProbeV5BaseURL
	rt.CheckBookIdentity(ctx)
	rt.StartCredentialFingerprints(declared...)
	rt.StartProviderPosture(declared...)
	return nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the boot manifest, provisioning it.
func approveSolanaSigner(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlays []string, mid billing.MerchantID, key string) error {
	rt := application.Runtime
	if rt.MerchantSecretBackend == nil || rt.Merchants == nil {
		return fmt.Errorf("no Vault Transit signer is configured")
	}
	if _, err := signeridentity.Approve(ctx, rt.DB, rt.Merchants, rt.MerchantSecretBackend.SolanaTransit, mid, config.ExpectedProviderEnvironment(config.IsTestMode(cfg)), key); err != nil {
		return err
	}
	if _, _, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, false); err != nil {
		return err
	}
	rt.ReportSignerKeyChange(nil)
	return nil
}

// reconcileBootMerchantManifest reports whether a Solana Transit signer fell
// back to its stored identity (or was deferred) because Vault did not answer.
func reconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlayPaths []string, tolerateVault bool) ([]string, bool, error) {
	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = bootstrap.DefaultMerchantConfigManifestPath
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is a boot-time CLI/config value, not request input
	if os.IsNotExist(err) {
		if explicit {
			return nil, false, fmt.Errorf("merchant manifest %s: %w (explicit startup manifest is required)", path, err)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read merchant manifest %s: %w", path, err)
	}

	overlays, err := bootstrap.ReadMerchantManifestOverlays(overlayPaths)
	if err != nil {
		return nil, false, err
	}
	manifest, err := bootstrap.LoadMerchantConfigManifestWithOverlays(raw, overlays...)
	if err != nil {
		return nil, false, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	rt := application.Runtime
	if rt == nil {
		return nil, false, fmt.Errorf("merchant startup requires a runtime")
	}
	opts := bootstrap.MerchantManifestReconcileOptions{StripeClients: rt.StripeClients, Insert: true}
	if config.SecretStoreBackend(cfg) == config.SecretBackendSnapshot {
		if rt.ManifestSecrets == nil {
			return nil, false, fmt.Errorf("snapshot credentials require the runtime snapshot plane")
		}
		opts.SecretStore = rt.ManifestSecrets.Seeder()
	}
	var signers []*signeridentity.Transit
	if backend := rt.MerchantSecretBackend; backend != nil && backend.SolanaTransit != nil && rt.Merchants != nil {
		opts.SolanaTransit = backend.SolanaTransit
		environment := config.ExpectedProviderEnvironment(config.IsTestMode(cfg))
		opts.WrapTransit = func(slug string, transit solanaint.TransitClient) solanaint.TransitClient {
			signer := &signeridentity.Transit{TransitClient: transit, DB: rt.DB, Directory: rt.Merchants, Slug: slug,
				Environment: environment, Tolerate: tolerateVault, OnChange: rt.ReportSignerKeyChange}
			signers = append(signers, signer)
			return signer
		}
		opts.DeferPSP = (&signeridentity.Transit{Tolerate: tolerateVault}).Defers
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest, opts); err != nil {
		return nil, false, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	log.WithField("file", path).Info("merchant startup initialized; existing metadata preserved")
	slugs := make([]string, 0, len(manifest.Merchants))
	for slug := range manifest.Merchants {
		slugs = append(slugs, slug)
	}
	pending := false
	for _, signer := range signers {
		pending = pending || signer.Unavailable()
	}
	return slugs, pending, nil
}
