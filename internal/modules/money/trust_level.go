package money

import (
	"context"
	"math"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
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

// TrustLevels are GetTrustLevel for many customers in one currency, in one
// read; a customer with none is absent.
func (s *MoneyService) TrustLevels(ctx context.Context, customers []uuid.UUID, currency string) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	cur := normalizeCurrency(currency)
	if len(customers) == 0 || IsMerchantUnit(cur) || RequireBillingCurrency(cur) != nil {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListCustomersMoneySettings(ctx, gen.ListCustomersMoneySettingsParams{MerchantID: mid.UUID(), CustomerIds: customers,
		RowLimit: int32(min(len(customers)*len(catalog.Currencies()), math.MaxInt32))}) // #nosec G115 -- clamped to int32
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Currency == cur && r.Tier != nil && *r.Tier != "" {
			out[r.CustomerID] = *r.Tier
		}
	}
	return out, nil
}
