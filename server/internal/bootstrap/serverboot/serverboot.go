// Package serverboot converges the standalone server's boot merchant manifest
// (#723) onto an engine built by openrails.New.
package serverboot

import (
	"context"
	"errors"
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
// Only Postgres and an unreadable manifest can fail it. A merchant it cannot
// provision is refused alone (Runtime.Refusals) and retried in the
// background; a document a merchant cannot serve is refused alone. With
// Vault holding merchant configuration, it is served once Vault answers. A
// Vault Transit signer uses the runtime's own client (logged in in the
// background): while Vault is down its PSP keeps the stored identity, or on a
// first boot is deferred and provisioned in the background once Vault
// answers; a changed Transit key fails closed until an operator approves it
// (see ApproveSolanaSigner). Posture is verified in the background;
// nmiProbeV5BaseURL is the test-only posture probe seam. overlays are files in
// the manifest's shape merged over it in order.
func ReconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlays []string, nmiProbeV5BaseURL string) error {
	rt := application.Runtime
	if rt == nil {
		return fmt.Errorf("merchant startup requires a runtime")
	}
	if err := rt.ServeMerchantsService(ctx); err != nil {
		return err
	}
	pass := &bootPass{rt: rt}
	slugs, pending, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, true, pass.refuse)
	if err != nil {
		return err
	}
	pass.settle()
	rt.ApproveSolanaSigner = func(ctx context.Context, mid billing.MerchantID, key string) error {
		return approveSolanaSigner(ctx, cfg, application, cp, path, overlays, mid, key)
	}
	if pending || len(pass.unprovisioned) > 0 {
		rt.Go("merchant manifest", func(ctx context.Context) {
			err := retry.Forever(ctx, func(ctx context.Context) error {
				if pending {
					if err := rt.Vault.State(); err != nil {
						return err
					}
				}
				next := &bootPass{rt: rt, previous: pass.refused}
				var err error
				if _, pending, err = reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, true, next.refuse); err != nil {
					return err
				}
				next.settle()
				pass = next
				switch {
				case len(next.unprovisioned) > 0:
					return fmt.Errorf("merchants %s are not provisioned", strings.Join(next.unprovisioned, ", "))
				case pending:
					return fmt.Errorf("the Solana Transit signer still awaits Vault")
				}
				return nil
			}, func(attempt int, err error) {
				if attempt == 0 {
					log.WithError(err).Warn("merchant startup: retrying in the background")
				}
			})
			if err == nil {
				log.Info("merchant startup: every merchant the manifest declares is provisioned")
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
			rt.NameMerchant(m.ID, slug)
		}
	}
	rt.NMIPostureV5BaseURL = nmiProbeV5BaseURL
	rt.CheckBookIdentity(ctx)
	rt.PreloadMerchantConfig(declared...)
	rt.StartProviderPosture(declared...)
	return nil
}

// bootPass collects what one manifest pass refused.
type bootPass struct {
	rt       *app.Runtime
	previous map[string]bool
	refused  map[string]bool
	// unprovisioned are the merchants the pass could not provision.
	unprovisioned []string
}

func (p *bootPass) refuse(subject string, err error) {
	if p.refused == nil {
		p.refused = map[string]bool{}
	}
	p.refused[subject] = true
	if errors.Is(err, bootstrap.ErrNotProvisioned) {
		p.unprovisioned = append(p.unprovisioned, strings.TrimPrefix(subject, "merchants."))
	}
	p.rt.Refuse(subject, err)
}

// settle clears what the previous pass refused and this one served.
func (p *bootPass) settle() {
	for subject := range p.previous {
		if !p.refused[subject] {
			p.rt.Unrefuse(subject)
		}
	}
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the boot manifest, provisioning it.
func approveSolanaSigner(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlays []string, mid billing.MerchantID, key string) error {
	rt := application.Runtime
	if rt.Vault == nil || rt.Vault.SolanaTransit == nil || rt.Merchants == nil {
		return fmt.Errorf("no Vault Transit signer is configured")
	}
	if _, err := signeridentity.Approve(ctx, rt.DB, rt.Merchants, rt.Vault.SolanaTransit, mid, key); err != nil {
		return err
	}
	if _, _, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, false, nil); err != nil {
		return err
	}
	rt.ReportSignerKeyChange(nil)
	return nil
}

// reconcileBootMerchantManifest reports whether a Solana Transit signer fell
// back to its stored identity (or was deferred) because Vault did not answer.
// refuse, when set, takes what a serving process serves without
// (bootstrap.ReconcileOptions.Refuse).
func reconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlayPaths []string, tolerateVault bool, refuse func(string, error)) ([]string, bool, error) {
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
	if rt.Merchants == nil {
		return nil, false, fmt.Errorf("merchant startup requires the merchant configuration")
	}
	opts := bootstrap.ReconcileOptions{Insert: true, Merchants: rt.Merchants, Refuse: refuse}
	var signers []*signeridentity.Transit
	if rt.Vault != nil && rt.Vault.SolanaTransit != nil {
		opts.SolanaTransit = rt.Vault.SolanaTransit
		opts.WrapTransit = func(slug string, transit solanaint.TransitClient) solanaint.TransitClient {
			signer := &signeridentity.Transit{TransitClient: transit, DB: rt.DB, Directory: rt.Merchants, Slug: slug,
				Tolerate: tolerateVault, OnChange: rt.ReportSignerKeyChange}
			signers = append(signers, signer)
			return signer
		}
		opts.DeferPSP = (&signeridentity.Transit{Tolerate: tolerateVault}).Defers
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest, opts); err != nil {
		return nil, false, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	log.WithField("file", path).Info("merchant startup: manifest applied")
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
