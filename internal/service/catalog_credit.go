package service

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func normalizeCreditGrant(spec *catalogwire.CreditGrantSpec) (*catalogwire.CreditGrantSpec, error) {
	if spec == nil {
		return nil, nil
	}
	out := *spec
	out.Currency = catalogwire.Currency(moneyutil.NormalizeCurrency(string(out.Currency)))
	unit := currency.IsUnitCode(string(out.Currency))
	if err := moneyutil.ValidateCurrency(string(out.Currency)); err != nil && !unit {
		return nil, apperr.Invalidf("credit_grant: %v", err)
	}
	if (out.Amount != nil) == out.FromPayment {
		return nil, apperr.Invalidf("credit_grant requires exactly one of amount or from_payment")
	}
	if unit && out.FromPayment {
		return nil, apperr.Invalidf("credit_grant.from_payment credits the payment's own currency: a pack of a merchant's unit names its amount")
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
	if price.TrialUnitAmount != nil || price.TrialDurationHours != nil {
		return apperr.Invalidf("credit products do not support trial prices")
	}
	if price.Quantity != nil {
		return apperr.Invalidf("credit products do not support per-seat prices")
	}
	if currency.IsUnitCode(string(price.Currency)) {
		return apperr.Invalidf("a credit product is bought with money, never with a merchant's unit")
	}
	// Any currency buys a pack of a merchant's unit, at the pack's amount.
	if !strings.EqualFold(string(credit.Currency), string(price.Currency)) && !currency.IsUnitCode(string(credit.Currency)) {
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

// Validate available prices against the current benefit. Historical prices stay
// intact; reactivation checks their compatibility before making them available.
func (s *Service) validateProductCreditUpdate(ctx context.Context, id billing.ProductID, credit *catalogwire.CreditGrantSpec) error {
	prices, err := s.ListPricesByProduct(ctx, id, true)
	if err != nil {
		return err
	}
	for _, price := range prices {
		if err := validateCreditPrice(credit, billing.CreatePriceParams{Currency: price.Currency, UnitAmount: price.UnitAmount, BillingIntervalHours: price.BillingIntervalHours, BillingIntervalMonths: price.BillingIntervalMonths, CustomerAmount: price.CustomerAmount,
			TrialUnitAmount: price.TrialUnitAmount, TrialDurationHours: price.TrialDurationHours, Quantity: price.Quantity}); err != nil {
			return err
		}
	}
	return nil
}
