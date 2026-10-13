package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
)

// nmiCatalogClient arms an NMI client for reading a provider price's plan:
// empty accountID is the merchant's active account, else that declared
// account. pspKey is the resolved account's key; ok=false is not armed.
func (s *Service) nmiCatalogClient(ctx context.Context, accountID string) (client *nmi.NMIClient, pspKey string, ok bool) {
	if s == nil || s.rt == nil || s.rt.RailConfigs == nil {
		return nil, "", false
	}
	proc, err := s.rt.RailConfigs.RailConfig(ctx, string(models.RailNMI), strings.TrimSpace(accountID))
	if err != nil || proc == nil || proc.NMI == nil || strings.TrimSpace(proc.NMI.SecurityKey) == "" {
		return nil, "", false
	}
	mid, err := merchant.Require(ctx)
	if err != nil || proc.ID == uuid.Nil {
		return nil, "", false
	}
	// The runtime's one NMI factory carries its transport.
	factory := railresolve.NMIFactory{Config: s.rt.Config}
	if s.rt.NMIClients != nil {
		factory = *s.rt.NMIClients
	}
	client, err = factory.ClientFor(mid, merchants.PSPScope{ID: proc.ID, Rail: string(models.RailNMI), AccountID: proc.EffectiveAccountID(), Key: proc.Key}, proc.ToNMIProviderSettings())
	if err != nil {
		return nil, "", false
	}
	return client, proc.Key, true
}
