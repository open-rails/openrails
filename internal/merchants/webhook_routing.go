package merchants

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantRouteUnresolved indicates an inbound webhook could not be mapped to an
// active tenant. The webhook handler MUST reject the request rather than fall
// back to a default merchant: routing the wrong tenant's signing secret would break
// the trust boundary.
var ErrMerchantRouteUnresolved = errors.New("tenancy: webhook host/slug maps to no active tenant")

// WebhookRoute is the resolved tenant for an inbound webhook, plus its routing
// metadata.
type WebhookRoute struct {
	MerchantID   merchant.ID
	MerchantSlug string
}

// WebhookResolver maps an explicit webhook path slug to a tenant. It is the
// FIRST step of webhook handling; the resolver is NOT the trust boundary — after
// resolution OpenRails loads that merchant's signing secret and verifies the
// signature.
type WebhookResolver interface {
	// ResolveBySlug maps an explicit merchant slug
	// to its tenant. Returns ErrMerchantRouteUnresolved for unknown/inactive tenants.
	ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error)
}

// ResolveBySlug implements WebhookResolver against the openrails.merchants directory.
func (s *Service) ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error) {
	slug = normalizeSlug(slug)
	if slug == "" {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	var idStr, status string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, status FROM openrails.merchants
		 WHERE slug = $1 AND deleted_at IS NULL
	`, slug).Scan(&idStr, &status)
	return s.routeFromScan(idStr, slug, status, err)
}

func (s *Service) routeFromScan(idStr, slug, status string, err error) (WebhookRoute, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WebhookRoute{}, ErrMerchantRouteUnresolved
		}
		return WebhookRoute{}, err
	}
	// A suspended tenant still RESOLVES (webhooks must keep being verified +
	// processed so historical billing state stays correct); only deleted tenants
	// (filtered above) are unroutable. The suspension write-gate applies to
	// merchant-admin mutations and service writes, not to processor webhooks.
	if MerchantStatus(status) == StatusDeleted {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	id, perr := merchant.ParseID(idStr)
	if perr != nil {
		return WebhookRoute{}, perr
	}
	return WebhookRoute{MerchantID: id, MerchantSlug: slug}, nil
}
