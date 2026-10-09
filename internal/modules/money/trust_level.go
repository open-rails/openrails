package money

import (
	"context"

	identity "github.com/open-rails/openrails/internal/billingidentity"
)

// GetTrustLevel returns the account's host-assigned trust level for one currency
// ("" if none).
func (s *MoneyService) GetTrustLevel(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return "", err
	}
	if settings.TrustLevel == nil {
		return "", nil
	}
	return *settings.TrustLevel, nil
}
