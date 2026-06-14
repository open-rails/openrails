package bootstrap

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/tenancy"
)

const tenantManifestAdvisoryLock = int64(734252042137424)

type TenantManifest struct {
	Version int              `yaml:"version"`
	Tenants []ManifestTenant `yaml:"tenants"`
}

type ManifestTenant struct {
	Slug        string `yaml:"slug"`
	Name        string `yaml:"name"`
	BillingTier string `yaml:"billing_tier"`
	Region      string `yaml:"region"`
	WebhookHost string `yaml:"webhook_host"`
	WebhookPath string `yaml:"webhook_path"`
	// OwnerTenantID is the AuthKit org (tenant) uuid that owns this merchant
	// (#481 ownership link). Optional.
	OwnerTenantID string `yaml:"owner_tenant_id"`
}

type TenantManifestReconcileOptions struct{}

// ReconcileTenantManifestData provisions the tenants, service-JWT grants, and
// issuers declared by a tenant manifest. It is the single tenant-provisioning
// entry point, consumed by the unified BootstrapManifest apply path (CLI +
// server first-run startup). Issuer registration is declarative and does not
// fetch the JWKS, so it succeeds even when the issuer's app is not yet running.
func ReconcileTenantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *TenantManifest, opts TenantManifestReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("tenant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("tenant bootstrap manifest is required")
	}
	if manifest.Version != BootstrapManifestVersion {
		return fmt.Errorf("tenant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	if err := lockTenantManifestBootstrap(ctx, cp); err != nil {
		return err
	}
	defer unlockTenantManifestBootstrap(context.Background(), cp)

	if len(manifest.Tenants) == 0 {
		log.Info("tenant bootstrap manifest has no tenants")
		return nil
	}

	svc, err := tenancy.NewService(cp.Pool(), nil)
	if err != nil {
		return err
	}

	for _, mt := range manifest.Tenants {
		tn, err := svc.Provision(ctx, tenancy.ProvisionRequest{
			Slug:          mt.Slug,
			Name:          mt.Name,
			OwnerTenantID: mt.OwnerTenantID,
			BillingTier:   mt.BillingTier,
			Region:        mt.Region,
			WebhookHost:   mt.WebhookHost,
			WebhookPath:   mt.WebhookPath,
		})
		if err != nil {
			return fmt.Errorf("tenant bootstrap: provision %q: %w", mt.Slug, err)
		}
		log.WithFields(log.Fields{
			"tenant":    tn.Slug,
			"tenant_id": tn.ID.String(),
		}).Info("tenant bootstrap: tenant ensured")
	}

	// #480/#481: issuer/JWKS trust is AuthKit's remote_application registry (#74),
	// not an OpenRails-owned table — the manifest no longer reconciles issuers.
	return nil
}

func lockTenantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) error {
	_, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_lock($1)`, tenantManifestAdvisoryLock)
	if err != nil {
		return fmt.Errorf("tenant bootstrap: acquire advisory lock: %w", err)
	}
	return nil
}

func unlockTenantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) {
	if _, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_unlock($1)`, tenantManifestAdvisoryLock); err != nil {
		log.WithError(err).Warn("tenant bootstrap: release advisory lock failed")
	}
}

// AnyTenantProvisioned reports whether any of the given tenant slugs is already
// provisioned in the control plane. The server uses this for first-run
// detection: it auto-applies the bootstrap manifest only when NONE of the
// manifest's tenants exist yet (#327). Checking the manifest's own slugs — not a
// blanket tenant count — is required because the control plane's own bootstrap
// always creates a "default" tenant, so the table is never empty.
func AnyTenantProvisioned(ctx context.Context, cp *controlplane.ControlPlane, slugs []string) (bool, error) {
	if cp == nil || cp.Pool() == nil {
		return false, nil
	}
	norm := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			norm = append(norm, s)
		}
	}
	if len(norm) == 0 {
		return false, nil
	}
	var exists bool
	err := cp.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.merchants WHERE slug = ANY($1) AND deleted_at IS NULL)`, norm).Scan(&exists)
	return exists, err
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
