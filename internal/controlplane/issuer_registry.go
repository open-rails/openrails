package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/pkg/tenant"
)

// DelegatedIssuer is one registered federated delegated-token issuer (issue
// #259): a tenant host backend that signs its own
// aud=openrails browser tokens and publishes a JWKS. OpenRails verifies
// tenant-signed tokens against the issuer's registered JWKS and resolves the
// OpenRails tenant from the validated `iss`.
type DelegatedIssuer struct {
	// Issuer is the token `iss`. GLOBALLY UNIQUE -> maps to exactly one tenant.
	Issuer string
	// JWKSURI is the ONLY URL OpenRails fetches this issuer's keys from
	// (allowlist; a token-supplied jwks_uri/iss is never fetched).
	JWKSURI string
	// Audiences are accepted JWT `aud` values for this issuer.
	Audiences []string
	// TenantID is the OpenRails tenant (#223) this issuer speaks for.
	TenantID tenant.ID
	// TenantSlug is that tenant's slug.
	TenantSlug string
	// Enabled is the per-issuer kill-switch state. Only enabled issuers are
	// loaded into the live verifier.
	Enabled bool
}

// ErrDelegatedIssuerUnknown indicates a presented token's validated `iss` is not
// a registered+enabled issuer mapped to an active tenant. Fail closed: the token
// is rejected even if its signature is well-formed.
var ErrDelegatedIssuerUnknown = errors.New("controlplane: delegated token issuer is not registered for an active tenant")

// loadEnabledDelegatedIssuers returns every enabled issuer whose tenant is
// active, joined to the tenant slug. This is the set registered into the
// multi-issuer verifier at startup and on reload. Issuers whose tenant is
// suspended/deleted are excluded (fail closed) even if the row is enabled.
func (c *ControlPlane) loadEnabledDelegatedIssuers(ctx context.Context) ([]DelegatedIssuer, error) {
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for issuer registry")
	}
	rows, err := c.pool.Query(ctx, `
		SELECT i.issuer, i.jwks_uri, i.audiences, i.tenant_id::text, t.slug, i.enabled
		  FROM billing.tenant_delegated_issuers i
		  JOIN billing.tenants t ON t.id = i.tenant_id
		 WHERE i.enabled
		   AND cardinality(i.audiences) > 0
		   AND t.status = 'active'
		   AND t.deleted_at IS NULL
		 ORDER BY i.issuer
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DelegatedIssuer
	for rows.Next() {
		var (
			issuer  string
			jwksURI string
			aud     []string
			tidStr  string
			slug    string
			enabled bool
		)
		if err := rows.Scan(&issuer, &jwksURI, &aud, &tidStr, &slug, &enabled); err != nil {
			return nil, err
		}
		tid, perr := tenant.ParseID(tidStr)
		if perr != nil {
			return nil, perr
		}
		out = append(out, DelegatedIssuer{
			Issuer:     strings.TrimSpace(issuer),
			JWKSURI:    strings.TrimSpace(jwksURI),
			Audiences:  cleanAudiences(aud),
			TenantID:   tid,
			TenantSlug: slug,
			Enabled:    enabled,
		})
	}
	return out, rows.Err()
}

// reloadDelegatedIssuers reconciles the live multi-issuer verifier with the
// registry: every enabled issuer (mapped to an active tenant) is registered via
// AddIssuer with JWKS-URL key fetching, and any issuer previously registered but
// no longer enabled is removed (the per-issuer kill-switch + reload convergence
// path, issue #259). The self-issuer is registered separately in
// newDelegatedVerifier (dual-trust migration window) and is never touched here.
//
// JWKS keys are fetched lazily by the verifier on first use and refreshed on the
// configured cadence / on unknown-kid, so registering an issuer whose JWKS is
// momentarily unreachable does not fail here — it fails closed per token at
// verify time. Registration-time reachability is validated by the service token-gated
// register route (issue #259 bootstrap), not here.
func (c *ControlPlane) reloadDelegatedIssuers(ctx context.Context) error {
	if c == nil || c.delegatedVerifier == nil {
		return ErrDelegatedNotConfigured
	}

	issuers, err := c.loadEnabledDelegatedIssuers(ctx)
	if err != nil {
		return err
	}

	c.issuerMu.Lock()
	defer c.issuerMu.Unlock()

	desired := make(map[string]struct{}, len(issuers))
	for _, iss := range issuers {
		if iss.Issuer == "" || iss.JWKSURI == "" {
			continue
		}
		// Never let a registered tenant issuer collide with the control plane's
		// own self-issuer (defense in depth; global-uniqueness + the register
		// route already prevent this).
		if iss.Issuer == strings.TrimSpace(c.issuer) {
			continue
		}
		// Re-registration must replace the JWKS cache. AuthKit's AddIssuer
		// upserts issuer metadata but intentionally preserves cached keys unless
		// explicit keys are supplied, so remove first to make issuer rotations
		// take effect immediately after registry reload.
		c.delegatedVerifier.RemoveIssuer(iss.Issuer)
		if err := c.delegatedVerifier.AddIssuer(iss.Issuer, iss.Audiences, authhttp.IssuerOptions{
			JWKSURI:  iss.JWKSURI,
			CacheTTL: delegatedIssuerJWKSCacheTTL,
		}); err != nil {
			return fmt.Errorf("controlplane: register delegated issuer %q: %w", iss.Issuer, err)
		}
		desired[iss.Issuer] = struct{}{}
	}

	// Evict issuers that were registered before but are no longer enabled/active.
	for prev := range c.delegatedIssuers {
		if _, keep := desired[prev]; !keep {
			c.delegatedVerifier.RemoveIssuer(prev)
		}
	}
	c.delegatedIssuers = desired
	return nil
}

// tenantForIssuer resolves the OpenRails tenant for a VALIDATED token issuer.
// The `iss` must already be signature-verified by the multi-issuer verifier; this
// maps it to the tenant via the registry, enforcing the issuer is enabled and its
// tenant active. Because `issuer` is globally unique, this is the issuer->tenant
// pin that makes no-cross-tenant-forgery hold: a given key can only ever resolve
// to its own tenant.
//
// Returns ErrDelegatedIssuerUnknown when the issuer is not registered, disabled,
// or maps to a non-active tenant (fail closed).
func (c *ControlPlane) tenantForIssuer(ctx context.Context, issuer string) (tenant.ID, string, error) {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return tenant.ID{}, "", ErrDelegatedIssuerUnknown
	}
	if c.pool == nil {
		return tenant.ID{}, "", errors.New("controlplane: pgx pool unavailable for issuer resolution")
	}

	var (
		tidStr string
		slug   string
	)
	err := c.pool.QueryRow(ctx, `
		SELECT t.id::text, t.slug
		  FROM billing.tenant_delegated_issuers i
		  JOIN billing.tenants t ON t.id = i.tenant_id
		 WHERE i.issuer = $1
		   AND i.enabled
		   AND t.status = 'active'
		   AND t.deleted_at IS NULL
		 LIMIT 1
	`, issuer).Scan(&tidStr, &slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tenant.ID{}, "", ErrDelegatedIssuerUnknown
		}
		return tenant.ID{}, "", err
	}
	tid, err := tenant.ParseID(tidStr)
	if err != nil {
		return tenant.ID{}, "", err
	}
	return tid, slug, nil
}
