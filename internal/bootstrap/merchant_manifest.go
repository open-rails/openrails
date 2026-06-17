package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

const merchantManifestAdvisoryLock = int64(734252042137424)

type MerchantManifest struct {
	Version   int                `yaml:"version"`
	Merchants []ManifestMerchant `yaml:"merchants"`
}

type ManifestMerchant struct {
	Slug        string `yaml:"slug"`
	Name        string `yaml:"name"`
	BillingTier string `yaml:"billing_tier"`
	Region      string `yaml:"region"`
	WebhookHost string `yaml:"webhook_host"`
	WebhookPath string `yaml:"webhook_path"`
	// OwnerOrgID is the AuthKit org uuid that owns this merchant namespace
	// (#500 ownership link). Optional in the manifest: when empty, bootstrap
	// creates/resolves an AuthKit org with the same slug as the merchant.
	OwnerOrgID string `yaml:"owner_org_id"`
	// Issuers is rejected during validation. Issuer/JWKS trust belongs in the
	// top-level auth.orgs[].issuers AuthKit bootstrap section.
	Issuers []any `yaml:"issuers,omitempty"`
}

type MerchantManifestReconcileOptions struct{}

// ReconcileMerchantManifestData provisions the merchants, service-JWT grants, and
// issuers declared by a merchant manifest. It is the single merchant-provisioning
// entry point, consumed by the unified BootstrapManifest apply path (CLI +
// server first-run startup). Issuer registration is declarative and does not
// fetch the JWKS, so it succeeds even when the issuer's app is not yet running.
func ReconcileMerchantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *MerchantManifest, opts MerchantManifestReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("merchant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("merchant bootstrap manifest is required")
	}
	if manifest.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	if err := lockMerchantManifestBootstrap(ctx, cp); err != nil {
		return err
	}
	defer unlockMerchantManifestBootstrap(context.Background(), cp)

	if len(manifest.Merchants) == 0 {
		log.Info("merchant bootstrap manifest has no merchants")
		return nil
	}

	svc, err := merchants.NewService(cp.Pool(), nil)
	if err != nil {
		return err
	}

	for _, mt := range manifest.Merchants {
		ownerOrgID := strings.TrimSpace(mt.OwnerOrgID)
		if ownerOrgID == "" {
			org, err := ensureManifestOwnerOrg(ctx, cp, mt.Slug)
			if err != nil {
				return fmt.Errorf("merchant bootstrap: ensure owner org for %q: %w", mt.Slug, err)
			}
			ownerOrgID = org.ID
		}
		tn, err := svc.Provision(ctx, merchants.ProvisionRequest{
			Slug:        mt.Slug,
			Name:        mt.Name,
			OwnerOrgID:  ownerOrgID,
			BillingTier: mt.BillingTier,
			Region:      mt.Region,
			WebhookHost: mt.WebhookHost,
			WebhookPath: mt.WebhookPath,
		})
		if err != nil {
			return fmt.Errorf("merchant bootstrap: provision %q: %w", mt.Slug, err)
		}
		log.WithFields(log.Fields{
			"merchant":    tn.Slug,
			"merchant_id": tn.ID.String(),
		}).Info("merchant bootstrap: merchant ensured")
	}

	// #480/#481: issuer/JWKS trust is AuthKit's remote_application registry (#74),
	// not an OpenRails-owned table — the manifest no longer reconciles issuers.
	return nil
}

func ensureManifestOwnerOrg(ctx context.Context, cp *controlplane.ControlPlane, slug string) (*authcore.Org, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("owner org slug is required")
	}
	org, err := cp.Core().ResolveOrgBySlug(ctx, slug)
	if err == nil {
		return org, nil
	}
	if !errors.Is(err, authcore.ErrOrgNotFound) {
		return nil, err
	}
	org, err = cp.Core().CreateOrg(ctx, slug)
	if err == nil {
		return org, nil
	}
	if !errors.Is(err, authcore.ErrOwnerSlugTaken) {
		return nil, err
	}
	return cp.Core().ResolveOrgBySlug(ctx, slug)
}

func lockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) error {
	_, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_lock($1)`, merchantManifestAdvisoryLock)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: acquire advisory lock: %w", err)
	}
	return nil
}

func unlockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) {
	if _, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_unlock($1)`, merchantManifestAdvisoryLock); err != nil {
		log.WithError(err).Warn("merchant bootstrap: release advisory lock failed")
	}
}

// AnyMerchantProvisioned reports whether any of the given org slugs is already
// provisioned in the control plane. The server uses this for first-run
// detection: it auto-applies the bootstrap manifest only when NONE of the
// manifest's merchants exist yet (#327). Checking the manifest's own slugs — not a
// blanket merchant count — is required because the control plane's own bootstrap
// always creates a "default" merchant, so the table is never empty.
func AnyMerchantProvisioned(ctx context.Context, cp *controlplane.ControlPlane, slugs []string) (bool, error) {
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
