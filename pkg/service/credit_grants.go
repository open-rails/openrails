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
	return s.moneyService().ListCreditGrants(ctx, payer, currency, limit, offset)
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
	return s.moneyService().RevokeCreditGrant(ctx, payer, grantID, reason, func(ctx context.Context, currency string) (int64, error) {
		if s.rt.RedisClient == nil {
			return 0, fmt.Errorf("credit revocation unavailable: live admission holds cannot be checked")
		}
		return spendgate.New(s.rt.RedisClient).HeldAmount(ctx, mid.String(), payer.UUID().String(), currency)
	})
}
