package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/eventlog"
	"github.com/open-rails/openrails/internal/merchant"
)

func (s *Service) ListEvents(ctx context.Context, q billing.EventQuery) (billing.EventPage, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.EventPage{}, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.EventPage{}, err
	}
	retained, err := config.EventRetentionDuration(s.rt.Config)
	if err != nil {
		return billing.EventPage{}, err
	}
	return eventlog.List(ctx, s.rt.DB, mid, q, retained, s.now())
}
