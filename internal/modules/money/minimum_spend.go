package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SetCustomerMinimumSpend sets (amount > 0) or clears (amount <= 0) a customer's
// per-period minimum-spend commitment for a billing currency (#643). At a
// periodic invoice close (FinalizeInvoice with WithMinimumSpendTrueUp), if the
// customer's rated period total is below this minimum, a true-up line brings the
// invoice up to it. A contract property of the billing relationship — NOT a
// per-line pricing floor (#642).
func (s *MoneyService) SetCustomerMinimumSpend(ctx context.Context, payer identity.CustomerID, currency string, amount int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if amount <= 0 {
			return q.DeleteCustomerMinimumSpend(ctx, gen.DeleteCustomerMinimumSpendParams{
				MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			})
		}
		// Materialize the payable customers row so the FK is satisfied even if no
		// prior money op touched this subject (#317).
		if err := ensureCustomer(ctx, q, tenantID, payer.UUID()); err != nil {
			return err
		}
		return q.UpsertCustomerMinimumSpend(ctx, gen.UpsertCustomerMinimumSpendParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			AmountMicros: amount, Now: now,
		})
	})
}

// GetCustomerMinimumSpend returns a customer's minimum-spend commitment for a
// currency, or 0 when none is set (#643).
func (s *MoneyService) GetCustomerMinimumSpend(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return 0, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	var amount int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		v, gerr := s.db.Gen(ctx).GetCustomerMinimumSpend(ctx, gen.GetCustomerMinimumSpendParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: cur,
		})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return nil
		}
		if gerr != nil {
			return gerr
		}
		amount = v
		return nil
	})
	return amount, err
}
