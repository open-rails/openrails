package service

import (
	"context"
	"time"

	"github.com/open-rails/openrails"
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

func (s *Service) ListOffersForEntitlement(ctx context.Context, key string, params openrails.OfferListParams) (*openrails.OfferList, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	return catalog.ListOffersForEntitlement(ctx, rt.DB, key, params)
}
