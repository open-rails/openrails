package tenancy

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrTenantRouteUnresolved indicates an inbound webhook could not be mapped to an
// active tenant. The webhook handler MUST reject the request rather than fall
// back to a default tenant: routing the wrong tenant's signing secret would break
// the trust boundary.
var ErrTenantRouteUnresolved = errors.New("tenancy: webhook host/slug maps to no active tenant")

// WebhookRoute is the resolved tenant for an inbound webhook, plus its routing
// metadata.
type WebhookRoute struct {
	MerchantID   merchant.ID
	TenantSlug string
}

// WebhookResolver maps an inbound webhook (host or explicit path slug) to a
// tenant. It is the FIRST step of webhook handling; the resolver is NOT the trust
// boundary — after resolution OpenRails loads that tenant's signing secret and
// verifies the signature. A managed ingress may instead resolve the tenant and
// forward a header, but OpenRails always re-derives and re-verifies.
type WebhookResolver interface {
	// ResolveBySlug maps an explicit tenant slug (e.g. from a /t/:tenant/ path)
	// to its tenant. Returns ErrTenantRouteUnresolved for unknown/inactive tenants.
	ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error)
	// ResolveByHost maps an inbound Host header to its tenant via the registered
	// webhook_host. Returns ErrTenantRouteUnresolved when no active tenant claims
	// the host.
	ResolveByHost(ctx context.Context, host string) (WebhookRoute, error)
}

// ResolveBySlug implements WebhookResolver against the openrails.tenants directory.
func (s *Service) ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error) {
	slug = normalizeSlug(slug)
	if slug == "" {
		return WebhookRoute{}, ErrTenantRouteUnresolved
	}
	var idStr, status string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, status FROM openrails.tenants
		 WHERE slug = $1 AND deleted_at IS NULL
	`, slug).Scan(&idStr, &status)
	return s.routeFromScan(idStr, slug, status, err)
}

// ResolveByHost implements WebhookResolver: maps a Host header to a tenant via the
// registered webhook_host. The default tenant is never matched by host (it has no
// registered host) so host routing is strictly opt-in per tenant.
func (s *Service) ResolveByHost(ctx context.Context, host string) (WebhookRoute, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	// Strip any port suffix from the Host header.
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return WebhookRoute{}, ErrTenantRouteUnresolved
	}
	var idStr, slug, status string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, slug, status FROM openrails.tenants
		 WHERE lower(webhook_host) = $1 AND deleted_at IS NULL
		 LIMIT 1
	`, host).Scan(&idStr, &slug, &status)
	return s.routeFromScan(idStr, slug, status, err)
}

func (s *Service) routeFromScan(idStr, slug, status string, err error) (WebhookRoute, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WebhookRoute{}, ErrTenantRouteUnresolved
		}
		return WebhookRoute{}, err
	}
	// A suspended tenant still RESOLVES (webhooks must keep being verified +
	// processed so historical billing state stays correct); only deleted tenants
	// (filtered above) are unroutable. The suspension write-gate applies to
	// tenant-admin mutations and service writes, not to processor webhooks.
	if TenantStatus(status) == StatusDeleted {
		return WebhookRoute{}, ErrTenantRouteUnresolved
	}
	id, perr := merchant.ParseID(idStr)
	if perr != nil {
		return WebhookRoute{}, perr
	}
	return WebhookRoute{MerchantID: id, TenantSlug: slug}, nil
}
