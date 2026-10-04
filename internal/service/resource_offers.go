package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

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
