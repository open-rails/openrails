package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/tenant"
)

// Issuer-registration errors (issue #259 bootstrap flow). All are sanitized for
// the service token-gated route to map to precise client responses.
var (
	// ErrIssuerOwnedByOtherTenant indicates the issuer is already registered to a
	// DIFFERENT tenant. Issuers are globally unique (-> exactly one tenant), so a
	// tenant can never claim another tenant's issuer.
	ErrIssuerOwnedByOtherTenant = errors.New("controlplane: issuer is already registered to another tenant")
	// ErrIssuerInvalidJWKSURI indicates the jwks_uri is malformed or fails the
	// SSRF allowlist (scheme/host policy).
	ErrIssuerInvalidJWKSURI = errors.New("controlplane: jwks_uri is invalid or not allowed")
	// ErrIssuerInvalidIssuer indicates the issuer string is empty/malformed.
	ErrIssuerInvalidIssuer = errors.New("controlplane: issuer is empty or invalid")
	// ErrIssuerAudiencesRequired indicates the issuer has no accepted `aud`
	// values. Audience checking is part of the OIDC trust contract.
	ErrIssuerAudiencesRequired = errors.New("controlplane: issuer audiences are required")
	// ErrIssuerNotFound indicates a disable/rotate targeted an issuer that is not
	// registered to the calling tenant.
	ErrIssuerNotFound = errors.New("controlplane: issuer not found for tenant")
)

// RegisterDelegatedIssuerParams describes a federated issuer to register/rotate
// (issue #259). TenantID is bound from the CALLING service token (resolved server-side) —
// never from the request body — so a caller can only register issuers under its
// OWN tenant.
type RegisterDelegatedIssuerParams struct {
	TenantID  tenant.ID
	Issuer    string
	JWKSURI   string
	Audiences []string
}

// RegisterDelegatedIssuer registers (or rotates) a federated delegated-token
// issuer for the caller's tenant and reloads the live verifier. It:
//   - validates the issuer + jwks_uri (SSRF allowlist),
//   - upserts the registry row enforcing GLOBAL issuer uniqueness (an issuer
//     already owned by another tenant is rejected),
//   - reloads the multi-issuer verifier so the issuer is trusted immediately.
//
// Registration does NOT fetch the JWKS: a trusted issuer is stored declaratively
// even if its app is not running yet, and the verifier fetches (and caches, with
// retry) the JWKS lazily the first time it must verify a token from that issuer.
// So a temporarily-unreachable issuer never blocks registration or bootstrap.
//
// Rotation (same tenant re-registering an existing issuer with a new jwks_uri)
// is just an upsert that also re-enables a previously-disabled issuer.
func (c *ControlPlane) RegisterDelegatedIssuer(ctx context.Context, p RegisterDelegatedIssuerParams) error {
	if c == nil || c.pool == nil {
		return ErrNoControlPlane
	}
	issuer := strings.TrimSpace(p.Issuer)
	if issuer == "" || issuer == strings.TrimSpace(c.issuer) {
		// Empty, or an attempt to shadow the control plane's own self-issuer.
		return ErrIssuerInvalidIssuer
	}
	jwksURI := strings.TrimSpace(p.JWKSURI)
	if err := c.validateJWKSURI(jwksURI); err != nil {
		return err
	}
	audiences := cleanAudiences(p.Audiences)
	if len(audiences) == 0 {
		return ErrIssuerAudiencesRequired
	}
	if (p.TenantID == tenant.ID{}) {
		return ErrServiceTokenTenantUnresolved
	}

	// Upsert with global-uniqueness enforcement. The DO UPDATE ... WHERE clause
	// only fires when the existing row belongs to the SAME tenant; a conflict
	// against another tenant's row leaves nothing updated and RETURNS no row,
	// which we surface as ErrIssuerOwnedByOtherTenant.
	var id string
	err := c.pool.QueryRow(ctx, `
		INSERT INTO billing.tenant_delegated_issuers (tenant_id, issuer, jwks_uri, audiences, enabled)
		VALUES ($1, $2, $3, $4, TRUE)
		ON CONFLICT (issuer) DO UPDATE
		   SET jwks_uri   = EXCLUDED.jwks_uri,
		       audiences  = EXCLUDED.audiences,
		       enabled    = TRUE,
		       updated_at = current_timestamp
		 WHERE billing.tenant_delegated_issuers.tenant_id = $1
		RETURNING id::text
	`, p.TenantID.String(), issuer, jwksURI, audiences).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIssuerOwnedByOtherTenant
		}
		return err
	}

	// Apply immediately to the live verifier.
	return c.reloadDelegatedIssuers(ctx)
}

// DisableDelegatedIssuer flips the per-issuer kill-switch for an issuer owned by
// the caller's tenant and evicts it from the live verifier WITHOUT affecting the
// tenant's other issuers (issue #259). Idempotent: disabling an already-disabled
// issuer succeeds. Returns ErrIssuerNotFound if the issuer is not registered to
// the caller's tenant (a tenant can never disable another tenant's issuer).
func (c *ControlPlane) DisableDelegatedIssuer(ctx context.Context, tenantID tenant.ID, issuer string) error {
	if c == nil || c.pool == nil {
		return ErrNoControlPlane
	}
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return ErrIssuerInvalidIssuer
	}
	if (tenantID == tenant.ID{}) {
		return ErrServiceTokenTenantUnresolved
	}

	ct, err := c.pool.Exec(ctx, `
		UPDATE billing.tenant_delegated_issuers
		   SET enabled = FALSE, updated_at = current_timestamp
		 WHERE issuer = $1 AND tenant_id = $2
	`, issuer, tenantID.String())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrIssuerNotFound
	}
	return c.reloadDelegatedIssuers(ctx)
}

// ListDelegatedIssuers returns the issuers registered to a tenant (enabled and
// disabled), for the management surface.
func (c *ControlPlane) ListDelegatedIssuers(ctx context.Context, tenantID tenant.ID) ([]DelegatedIssuer, error) {
	if c == nil || c.pool == nil {
		return nil, ErrNoControlPlane
	}
	rows, err := c.pool.Query(ctx, `
		SELECT issuer, jwks_uri, audiences, enabled
		  FROM billing.tenant_delegated_issuers
		 WHERE tenant_id = $1
		 ORDER BY issuer
	`, tenantID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegatedIssuer
	for rows.Next() {
		var d DelegatedIssuer
		if err := rows.Scan(&d.Issuer, &d.JWKSURI, &d.Audiences, &d.Enabled); err != nil {
			return nil, err
		}
		d.TenantID = tenantID
		out = append(out, d)
	}
	return out, rows.Err()
}

func cleanAudiences(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// validateJWKSURI enforces the SSRF allowlist for a registered jwks_uri. The URL
// is the ONLY one OpenRails ever fetches for the issuer, so it must be safe:
//   - https scheme (http permitted ONLY outside production, for local/test),
//   - a host that does not resolve to a private/loopback/link-local address.
//
// A token-supplied jwks_uri/iss URL is NEVER fetched (the verifier only fetches
// the registered URL), so this is the single chokepoint where an unsafe target
// could be introduced — it fails closed.
func (c *ControlPlane) validateJWKSURI(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrIssuerInvalidJWKSURI
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrIssuerInvalidJWKSURI
	}
	prod := c.isProdEnv()
	switch u.Scheme {
	case "https":
	case "http":
		if prod {
			return fmt.Errorf("%w: https required in production", ErrIssuerInvalidJWKSURI)
		}
	default:
		return ErrIssuerInvalidJWKSURI
	}
	host := u.Hostname()
	if host == "" {
		return ErrIssuerInvalidJWKSURI
	}
	// In production, reject hosts that are IP literals in a private/loopback/
	// link-local range. (Outside production we allow them so httptest/localhost
	// work.) DNS-rebinding-grade protection would re-check at fetch time; this is
	// the registration-time allowlist guard.
	if prod {
		if ip := net.ParseIP(host); ip != nil && isDisallowedIP(ip) {
			return fmt.Errorf("%w: private/loopback host not allowed", ErrIssuerInvalidJWKSURI)
		}
		if isLoopbackHostname(host) {
			return fmt.Errorf("%w: loopback host not allowed", ErrIssuerInvalidJWKSURI)
		}
	}
	return nil
}

// isProdEnv reports whether this deployment runs in a production environment,
// where the JWKS-URI SSRF allowlist is strict (https + non-private host).
func (c *ControlPlane) isProdEnv() bool {
	if c == nil || c.cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.cfg.Env)) {
	case "production", "prod":
		return true
	default:
		return false
	}
}

// isDisallowedIP reports whether ip is in a range a JWKS URL must not target.
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func isLoopbackHostname(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}
