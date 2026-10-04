package service

import (
	"context"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func (s *Service) CheckEntitlements(ctx context.Context, customer string, keys []string, at time.Time) (map[string]bool, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	return rt.EntitlementService.CheckMany(ctx, customer, keys, at)
}

// ListOffers returns a page of live offers per requested entitlement.
func (s *Service) ListOffers(ctx context.Context, params billing.OfferListParams) (billing.OfferPages, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	return catalog.ListOffers(ctx, rt.DB, params)
}
