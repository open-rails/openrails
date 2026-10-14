package service

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/railresolve"
)

// stripeCatalogReader reads a Stripe provider price: empty accountID is the
// merchant's active account, else that declared account's. ok=false when no
// usable credentials arm it.
func (s *Service) stripeCatalogReader(ctx context.Context, accountID string) (*catalog.StripeCatalogService, bool) {
	if s == nil || s.rt == nil || s.rt.Config == nil || s.rt.RailConfigs == nil {
		return nil, false
	}
	if strings.TrimSpace(accountID) == "" {
		if !s.railArmed(ctx, string(models.RailStripe)) {
			return nil, false
		}
		return &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}, true
	}
	proc, err := s.rt.RailConfigs.RailConfig(ctx, string(models.RailStripe), accountID)
	if err != nil || proc == nil || proc.Stripe == nil || strings.TrimSpace(proc.Stripe.SecretKey) == "" {
		return nil, false
	}
	return &catalog.StripeCatalogService{
		Config:        s.rt.Config,
		StripeClients: s.rt.StripeClients,
		Rails:         railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: proc.Stripe}},
	}, true
}
