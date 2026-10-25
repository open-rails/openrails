package merchants

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
)

// CustodianConfigurations projects only the selected merchant's own vaults;
// platform custody has a separate deployment-owned configuration boundary.
func (s *Service) CustodianConfigurations(ctx context.Context, merchant billing.MerchantID) ([]billing.Custodian, error) {
	scopes, err := s.ListCustodians(ctx, merchant)
	if err != nil {
		return nil, err
	}
	out := make([]billing.Custodian, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Kind == models.CustodianPlatform {
			continue
		}
		credentials := map[string]billing.PSPCredential{}
		if descriptor, ok := custodians.Get(scope.Kind); ok {
			for _, slot := range descriptor.Secrets {
				if slot.MerchantWritable {
					credentials[slot.Name] = billing.PSPCredential{Configured: scope.Secret(slot.Name) != ""}
				}
			}
		}
		out = append(out, billing.Custodian{Key: scope.Key, Kind: scope.Kind, Environment: scope.Environment, AccountID: scope.AccountID, Settings: maps.Clone(scope.Settings), Credentials: credentials, Archived: scope.Archived, Revision: scope.Revision})
	}
	slices.SortFunc(out, func(a, b billing.Custodian) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}
