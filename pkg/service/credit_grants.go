package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

type CreditGrant = money.CreditGrant
type CreditGrantPage = money.CreditGrantPage
type CreditGrantRevocation = money.CreditGrantRevocation

var (
	ErrCreditGrantNotFound    = money.ErrCreditGrantNotFound
	ErrCreditGrantUnavailable = money.ErrCreditGrantUnavailable
	ErrCreditGrantHeld        = money.ErrCreditGrantHeld
)

func (s *Service) ListCreditGrants(ctx context.Context, payer identity.CustomerID, currency string, limit, offset int) (*CreditGrantPage, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	code, err := s.resolveCurrency(ctx, currency)
	if err != nil {
		return nil, err
	}
	page, err := s.moneyService().ListCreditGrants(ctx, payer, code, limit, offset)
	if err != nil {
		return nil, err
	}
	display, err := s.DisplayCurrency(ctx, code)
	if err != nil {
		return nil, err
	}
	for i := range page.Grants {
		page.Grants[i].Currency = display
	}
	return page, nil
}

func (s *Service) RevokeCreditGrant(ctx context.Context, payer identity.CustomerID, grantID uuid.UUID, reason string) (*CreditGrantRevocation, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := s.moneyService().RevokeCreditGrant(ctx, payer, grantID, reason)
	if err != nil {
		return nil, err
	}
	result.Grant.Currency, err = s.DisplayCurrency(ctx, result.Grant.Currency)
	return result, err
}

// CreditUnitDecimals returns the existing registry's scale for a selected unit.
func (s *Service) CreditUnitDecimals(ctx context.Context, currency string) (int, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	code, err := s.resolveCurrency(ctx, currency)
	if err != nil {
		return 0, err
	}
	decimals, err := money.CurrencyDecimals(code)
	return decimals, err
}
