package merchants

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantRouteUnresolved indicates an inbound webhook could not be mapped to an
// active merchant. The webhook handler MUST reject the request rather than fall
// back to a default merchant: routing the wrong merchant's signing secret would break
// the trust boundary.
var ErrMerchantRouteUnresolved = errors.New("merchants: webhook host/slug maps to no active merchant")

// WebhookRoute is the resolved merchant for an inbound webhook, plus its routing
// metadata.
type WebhookRoute struct {
	MerchantID   merchant.ID
	MerchantSlug string
}

// WebhookResolver maps an explicit webhook path slug to a merchant. It is the
// FIRST step of webhook handling; the resolver is NOT the trust boundary — after
// resolution OpenRails loads that merchant's signing secret and verifies the
// signature.
type WebhookResolver interface {
	// ResolveBySlug maps an explicit merchant slug
	// to its merchant. Returns ErrMerchantRouteUnresolved for unknown/inactive merchants.
	ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error)
}

// ResolveBySlug uses the same authoritative name resolution as other merchant
// entrypoints. Provider signature verification still follows resolution.
func (s *Service) ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error) {
	if normalizeSlug(slug) == "" {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	m, err := s.GetBySlug(ctx, slug)
	if errors.Is(err, ErrMerchantNotFound) || errors.Is(err, ErrMerchantRetired) {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	if err != nil {
		return WebhookRoute{}, err
	}
	if m.Status != StatusActive {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	return WebhookRoute{MerchantID: m.ID, MerchantSlug: m.Slug}, nil
}
