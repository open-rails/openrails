package service

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func normalizeCreditGrant(spec *catalogwire.CreditGrantSpec) (*catalogwire.CreditGrantSpec, error) {
	if spec == nil {
		return nil, nil
	}
	out := *spec
	out.Currency = moneyutil.NormalizeCurrency(out.Currency)
	if err := moneyutil.ValidateCurrency(out.Currency); err != nil {
		return nil, apperr.Invalidf("credit_grant: %v", err)
	}
	if (out.Amount != nil) == out.FromPayment {
		return nil, apperr.Invalidf("credit_grant requires exactly one of amount or from_payment")
	}
	if out.Amount != nil {
		if *out.Amount <= 0 {
			return nil, apperr.Invalidf("credit_grant.amount must be positive")
		}
		amount := *out.Amount
		out.Amount = &amount
	}
	days := 365
	if out.ExpiresAfterDays != nil {
		days = *out.ExpiresAfterDays
	}
	if days <= 0 || days > 36500 {
		return nil, apperr.Invalidf("credit_grant.expires_after_days must be between 1 and 36500")
	}
	out.ExpiresAfterDays = &days
	return &out, nil
}

func validateCreditPrice(credit *catalogwire.CreditGrantSpec, price billing.CreatePriceParams) error {
	if price.Archived {
		return nil
	}
	if credit == nil {
		if price.CustomerAmount != nil {
			return apperr.Invalidf("customer_amount requires a credit deposit product")
		}
		return nil
	}
	if price.AutoRenew {
		return apperr.Invalidf("recurring credit grants are not supported")
	}
	if !strings.EqualFold(credit.Currency, price.Currency) {
		return apperr.Invalidf("credit_grant currency must match the price currency")
	}
	if price.CustomerAmount != nil && !credit.FromPayment {
		return apperr.Invalidf("customer_amount requires credit_grant.from_payment")
	}
	if price.CustomerAmount == nil && price.UnitAmount <= 0 {
		return apperr.Invalidf("purchased credit prices must have a positive amount")
	}
	return nil
}

// Validate available offers against the current benefit. Historical prices stay
// intact; reactivation checks their compatibility before making them available.
func (s *Service) validateProductCreditUpdate(ctx context.Context, id billing.ProductID, credit *catalogwire.CreditGrantSpec) error {
	prices, err := s.ListPricesByProduct(ctx, id, true)
	if err != nil {
		return err
	}
	for _, price := range prices {
		if err := validateCreditPrice(credit, billing.CreatePriceParams{Currency: price.Currency, UnitAmount: price.UnitAmount, AutoRenew: price.AutoRenew, CustomerAmount: price.CustomerAmount}); err != nil {
			return err
		}
	}
	return nil
}
