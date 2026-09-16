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
	code, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	page, err := s.moneyService().ListCreditGrants(ctx, payer, code, limit, offset)
	if err != nil {
		return nil, err
	}
	for i := range page.Grants {
		page.Grants[i].Currency = code
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
	return result, nil
}
