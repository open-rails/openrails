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
	"github.com/open-rails/openrails/internal/retry"
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
// Vault holding merchant configuration, it is served once Vault answers.
// Posture is verified in the background;
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
	slugs, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, pass.refuse)
	if err != nil {
		return err
	}
	pass.settle()
	if len(pass.unprovisioned) > 0 {
		rt.Go("merchant manifest", func(ctx context.Context) {
			err := retry.Forever(ctx, func(ctx context.Context) error {
				next := &bootPass{rt: rt, previous: pass.refused}
				if _, err := reconcileBootMerchantManifest(ctx, cfg, application, cp, path, overlays, next.refuse); err != nil {
					return err
				}
				next.settle()
				pass = next
				if len(next.unprovisioned) > 0 {
					return fmt.Errorf("merchants %s are not provisioned", strings.Join(next.unprovisioned, ", "))
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

// reconcileBootMerchantManifest applies the boot manifest, returning the
// slugs it declares. refuse, when set, takes what a serving process serves
// without (bootstrap.ReconcileOptions.Refuse).
func reconcileBootMerchantManifest(ctx context.Context, cfg *config.Config, application *app.App, cp *controlplane.ControlPlane, path string, overlayPaths []string, refuse func(string, error)) ([]string, error) {
	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = bootstrap.DefaultMerchantConfigManifestPath
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is a boot-time CLI/config value, not request input
	if os.IsNotExist(err) {
		if explicit {
			return nil, fmt.Errorf("merchant manifest %s: %w (explicit startup manifest is required)", path, err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read merchant manifest %s: %w", path, err)
	}

	overlays, err := bootstrap.ReadMerchantManifestOverlays(overlayPaths)
	if err != nil {
		return nil, err
	}
	manifest, err := bootstrap.LoadMerchantConfigManifestWithOverlays(raw, overlays...)
	if err != nil {
		return nil, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	rt := application.Runtime
	if rt == nil {
		return nil, fmt.Errorf("merchant startup requires a runtime")
	}
	if rt.Merchants == nil {
		return nil, fmt.Errorf("merchant startup requires the merchant configuration")
	}
	opts := bootstrap.ReconcileOptions{Insert: true, Merchants: rt.Merchants, Refuse: refuse}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest, opts); err != nil {
		return nil, fmt.Errorf("merchant manifest %s: %w", path, err)
	}
	log.WithField("file", path).Info("merchant startup: manifest applied")
	slugs := make([]string, 0, len(manifest.Merchants))
	for slug := range manifest.Merchants {
		slugs = append(slugs, slug)
	}
	return slugs, nil
}
