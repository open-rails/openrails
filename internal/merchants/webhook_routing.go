package merchants

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/billing"
)

// ErrMerchantRouteUnresolved indicates an inbound webhook could not be mapped to an
// active merchant. The webhook handler MUST reject the request rather than fall
// back to a default merchant: routing the wrong merchant's signing secret would break
// the trust boundary.
var ErrMerchantRouteUnresolved = errors.New("merchants: webhook host/slug maps to no active merchant")

// WebhookRoute is the resolved merchant for an inbound webhook, plus its routing
// metadata.
type WebhookRoute struct {
	MerchantID   billing.MerchantID
	MerchantSlug string
}

// ResolveBySlug uses the same authoritative name resolution as other merchant
// entrypoints. Provider signature verification still follows resolution.
func (s *Service) ResolveBySlug(ctx context.Context, slug string) (WebhookRoute, error) {
	if normalizeSlug(slug) == "" {
		return WebhookRoute{}, ErrMerchantRouteUnresolved
	}
	m, err := s.GetBySlug(ctx, slug)
	if errors.Is(err, billing.ErrMerchantNotFound) || errors.Is(err, ErrMerchantRetired) {
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
