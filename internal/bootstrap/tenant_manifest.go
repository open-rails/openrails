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
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

const tenantManifestAdvisoryLock = int64(734252042137424)

type TenantManifest struct {
	Version int              `yaml:"version"`
	Tenants []ManifestTenant `yaml:"tenants"`
}

type ManifestTenant struct {
	Slug                 string                        `yaml:"slug"`
	Name                 string                        `yaml:"name"`
	BillingTier          string                        `yaml:"billing_tier"`
	Region               string                        `yaml:"region"`
	WebhookHost          string                        `yaml:"webhook_host"`
	WebhookPath          string                        `yaml:"webhook_path"`
	Issuers              []ManifestIssuer              `yaml:"issuers"`
	ServiceJWTPrincipals []ManifestServiceJWTPrincipal `yaml:"service_jwt_principals"`
}

type ManifestIssuer struct {
	Issuer    string   `yaml:"issuer"`
	JWKSURI   string   `yaml:"jwks_uri"`
	Audiences []string `yaml:"audiences"`
	Enabled   *bool    `yaml:"enabled"`
}

type ManifestServiceJWTPrincipal struct {
	Issuer      string             `yaml:"issuer"`
	Subject     string             `yaml:"subject"`
	Permissions []string           `yaml:"permissions"`
	Resources   []ManifestResource `yaml:"resources"`
	Enabled     *bool              `yaml:"enabled"`
}

type ManifestResource struct {
	Kind string `yaml:"kind"`
	ID   string `yaml:"id"`
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

		for _, principal := range mt.ServiceJWTPrincipals {
			if err := ensureManifestServiceJWTGrant(ctx, cp, tn.ID, tn.Slug, principal); err != nil {
				return fmt.Errorf("tenant bootstrap: service JWT principal %q for tenant %q: %w", principal.Subject, tn.Slug, err)
			}
		}
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

func ensureManifestServiceJWTGrant(ctx context.Context, cp *controlplane.ControlPlane, tenantID tenant.ID, tenantSlug string, principal ManifestServiceJWTPrincipal) error {
	issuer := strings.TrimSpace(principal.Issuer)
	if issuer == "" {
		return errors.New("issuer is required")
	}
	subject := strings.TrimSpace(principal.Subject)
	if subject == "" {
		return errors.New("subject is required")
	}
	permissions := cleanStrings(principal.Permissions)
	if len(permissions) == 0 {
		return fmt.Errorf("permissions are required for service JWT principal %q", subject)
	}
	resources, err := resolveManifestResources(principal.Resources, tenantID, tenantSlug)
	if err != nil {
		return err
	}
	enabled := true
	if principal.Enabled != nil {
		enabled = *principal.Enabled
	}
	return cp.UpsertServiceJWTGrant(ctx, controlplane.ServiceJWTGrant{
		TenantID:    tenantID,
		Issuer:      issuer,
		Subject:     subject,
		Permissions: permissions,
		Resources:   resources,
		Enabled:     enabled,
	})
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

func resolveManifestResources(in []ManifestResource, tenantID tenant.ID, tenantSlug string) ([]authcore.ServiceTokenResource, error) {
	if len(in) == 0 {
		return []authcore.ServiceTokenResource{controlplane.TenantResource(tenantID)}, nil
	}
	out := make([]authcore.ServiceTokenResource, 0, len(in))
	for _, r := range in {
		kind := strings.TrimSpace(r.Kind)
		id := strings.TrimSpace(r.ID)
		if kind == "" || id == "" {
			return nil, errors.New("resource kind and id are required")
		}
		if kind == controlplane.ResourceKindTenant {
			switch strings.ToLower(id) {
			case "$tenant", "self", tenantSlug:
				id = tenantID.String()
			}
		}
		out = append(out, authcore.ServiceTokenResource{Kind: kind, ID: id})
	}
	return out, nil
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
			err := cp.RegisterDelegatedIssuer(ctx, controlplane.RegisterDelegatedIssuerParams{
				TenantID:  tid,
				Issuer:    issuer.Issuer,
				JWKSURI:   issuer.JWKSURI,
				Audiences: issuer.Audiences,
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

// HasProvisionedTenants reports whether any tenant has been provisioned in the
// control plane. The server uses this for first-run detection: it auto-applies
// the bootstrap manifest only when no tenants exist yet (#327).
func HasProvisionedTenants(ctx context.Context, cp *controlplane.ControlPlane) (bool, error) {
	if cp == nil || cp.Pool() == nil {
		return false, nil
	}
	var exists bool
	err := cp.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.tenants WHERE deleted_at IS NULL)`).Scan(&exists)
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
