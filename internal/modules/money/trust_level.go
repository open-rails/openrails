package money

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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

// SetTrustLevelOverride sets the host-assigned level. An empty level clears it.
func (s *MoneyService) SetTrustLevelOverride(ctx context.Context, payer identity.CustomerID, currency, trustLevel string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), cur, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetMoneyAccountTier(ctx, gen.SetMoneyAccountTierParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			Tier: trustLevel, Now: now,
		})
	})
}
