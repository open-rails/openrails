package service

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.moneyService().RevokeCreditGrant(ctx, payer, grantID, reason, func(ctx context.Context, currency string) (int64, error) {
		if s.rt.RedisClient == nil {
			return 0, fmt.Errorf("credit revocation unavailable: live admission holds cannot be checked")
		}
		return spendgate.New(s.rt.RedisClient).HeldAmount(ctx, mid.String(), payer.UUID().String(), currency)
	})
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
	decimals, _, err := s.moneyService().ResolveUnit(ctx, code)
	return decimals, err
}
