package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/money"
)

var (
	ErrCreditGrantNotFound    = money.ErrCreditGrantNotFound
	ErrCreditGrantUnavailable = money.ErrCreditGrantUnavailable
	ErrCreditGrantHeld        = money.ErrCreditGrantHeld
)

// CreateCreditGrant grants a customer prepaid credit, idempotent on the
// customer and SourceID.
func (s *Service) CreateCreditGrant(ctx context.Context, customer identity.CustomerID, params billing.CreateCreditGrantParams) (*billing.CreditGrant, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	key, err := money.NewIdempotencyKey(money.OpDeposit, params.Source, params.SourceID)
	if err != nil {
		return nil, err
	}
	invoker := strings.TrimSpace(params.Invoker)
	if invoker == "" {
		invoker = customer.String()
	}
	sourceID := key.SourceID()
	var expiresAt = params.ExpiresAt
	if expiresAt != nil {
		v := expiresAt.UTC()
		expiresAt = &v
	}
	trx, err := s.moneyService().Deposit(ctx, money.DepositParams{
		CustomerID: &customer, Invoker: invoker, Currency: currency, Amount: params.Amount,
		Source: key.Source(), SourceID: &sourceID, ExpiresAt: expiresAt, Description: params.Description,
	})
	if err != nil {
		return nil, err
	}
	grant, err := s.moneyService().GetCreditGrant(ctx, customer, trx.ID)
	if err != nil {
		return nil, err
	}
	grant.Replayed = trx.Replayed
	return grant, nil
}

// ListCreditGrants lists a customer's credit grants, newest first.
func (s *Service) ListCreditGrants(ctx context.Context, customer identity.CustomerID, params billing.CreditGrantListParams) (billing.ListPage[billing.CreditGrant], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.CreditGrant]{}, err
	}
	defer release()
	return s.moneyService().ListCreditGrants(ctx, customer, params)
}

// GetCreditGrant reads one of a customer's credit grants.
func (s *Service) GetCreditGrant(ctx context.Context, customer identity.CustomerID, id billing.CreditGrantID) (*billing.CreditGrant, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.moneyService().GetCreditGrant(ctx, customer, id.UUID())
}

// RevokeCreditGrant revokes a grant's unspent remainder.
func (s *Service) RevokeCreditGrant(ctx context.Context, customer identity.CustomerID, id billing.CreditGrantID, reason string) (*billing.CreditGrant, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.moneyService().RevokeCreditGrant(ctx, customer, id.UUID(), reason)
}

// ListCreditTransactions lists a customer's ledger in one currency, newest
// first.
func (s *Service) ListCreditTransactions(ctx context.Context, customer identity.CustomerID, params billing.CreditTransactionListParams) (billing.ListPage[billing.CreditTransaction], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.CreditTransaction]{}, err
	}
	defer release()
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return billing.ListPage[billing.CreditTransaction]{}, err
	}
	params.Currency = currency
	return s.moneyService().ListCreditTransactions(ctx, customer, params)
}
