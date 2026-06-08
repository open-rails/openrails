package bootstrap

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

const tenantManifestAdvisoryLock = int64(734252042137424)

type TenantManifest struct {
	Version int              `yaml:"version"`
	Tenants []ManifestTenant `yaml:"tenants"`
}

type ManifestTenant struct {
	Slug        string           `yaml:"slug"`
	Name        string           `yaml:"name"`
	BillingTier string           `yaml:"billing_tier"`
	Region      string           `yaml:"region"`
	WebhookHost string           `yaml:"webhook_host"`
	WebhookPath string           `yaml:"webhook_path"`
	Issuers     []ManifestIssuer `yaml:"issuers"`
}

type ManifestIssuer struct {
	Issuer    string   `yaml:"issuer"`
	JWKSURI   string   `yaml:"jwks_uri"`
	Audiences []string `yaml:"audiences"`
	Enabled   *bool    `yaml:"enabled"`
}

// defaultIssuerAudience is the JWT `aud` value accepted for a registered issuer
// when the manifest declares none. Every Doujins-stack issuer mints tokens with
// aud=openrails, so registration need not restate it per issuer.
const defaultIssuerAudience = "openrails"

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

	svc, err := tenancy.NewService(cp.Pool(), cp, nil)
	if err != nil {
		return err
	}

	for _, mt := range manifest.Tenants {
		tn, err := svc.Provision(ctx, tenancy.ProvisionRequest{
			Slug:        mt.Slug,
			Name:        mt.Name,
			BillingTier: mt.BillingTier,
			Region:      mt.Region,
			WebhookHost: mt.WebhookHost,
			WebhookPath: mt.WebhookPath,
		})
		if err != nil {
			return fmt.Errorf("tenant bootstrap: provision %q: %w", mt.Slug, err)
		}
		log.WithFields(log.Fields{
			"tenant":    tn.Slug,
			"tenant_id": tn.ID.String(),
		}).Info("tenant bootstrap: tenant ensured")
	}

	// Issuer registration is declarative and does not fetch the JWKS, so it
	// succeeds even when the issuer's app is not running yet (the verifier fetches
	// the JWKS lazily at token-verification time). A failure here is a genuine
	// config/DB error, so surface it.
	if ok := reconcileManifestIssuersOnce(ctx, cp, manifest); !ok {
		return fmt.Errorf("tenant bootstrap: issuer registration failed")
	}
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

// reconcileManifestIssuersOnce registers (or disables) every tenant's declared
// issuers. Registration is declarative and does not probe the JWKS, so it does
// not depend on the issuer's app being reachable. Returns false if any genuine
// registration error occurred (invalid config, ownership conflict, DB error).
func reconcileManifestIssuersOnce(ctx context.Context, cp *controlplane.ControlPlane, manifest *TenantManifest) bool {
	allDone := true
	for _, mt := range manifest.Tenants {
		if len(mt.Issuers) == 0 {
			continue
		}
		tid, err := tenantIDForSlug(ctx, cp, mt.Slug)
		if err != nil {
			log.WithError(err).WithField("tenant", mt.Slug).Warn("tenant bootstrap: issuer reconciliation waiting for tenant")
			allDone = false
			continue
		}
		for _, issuer := range mt.Issuers {
			if issuer.Enabled != nil && !*issuer.Enabled {
				if err := cp.DisableDelegatedIssuer(ctx, tid, issuer.Issuer); err != nil {
					log.WithError(err).WithField("issuer", issuer.Issuer).Warn("tenant bootstrap: disable issuer failed")
					allDone = false
				}
				continue
			}
			audiences := cleanStrings(issuer.Audiences)
			if len(audiences) == 0 {
				audiences = []string{defaultIssuerAudience}
			}
			err := cp.RegisterDelegatedIssuer(ctx, controlplane.RegisterDelegatedIssuerParams{
				TenantID:  tid,
				Issuer:    issuer.Issuer,
				JWKSURI:   issuer.JWKSURI,
				Audiences: audiences,
			})
			if err != nil {
				log.WithError(err).WithFields(log.Fields{"tenant": mt.Slug, "issuer": issuer.Issuer, "jwks_uri": issuer.JWKSURI}).
					Error("tenant bootstrap: issuer registration failed")
				allDone = false
				continue
			}
			log.WithFields(log.Fields{"tenant": mt.Slug, "issuer": issuer.Issuer}).Info("tenant bootstrap: issuer registered")
		}
	}
	return allDone
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
	err := cp.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.tenants WHERE slug = ANY($1) AND deleted_at IS NULL)`, norm).Scan(&exists)
	return exists, err
}

func tenantIDForSlug(ctx context.Context, cp *controlplane.ControlPlane, slug string) (tenant.ID, error) {
	var id string
	err := cp.Pool().QueryRow(ctx, `
		SELECT id::text FROM billing.tenants
		 WHERE slug = $1 AND status = 'active' AND deleted_at IS NULL
		 LIMIT 1
	`, strings.ToLower(strings.TrimSpace(slug))).Scan(&id)
	if err != nil {
		return tenant.ID{}, err
	}
	return tenant.ParseID(id)
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
