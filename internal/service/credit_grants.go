package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

var (
	ErrCreditGrantNotFound    = money.ErrCreditGrantNotFound
	ErrCreditGrantUnavailable = money.ErrCreditGrantUnavailable
	ErrCreditGrantHeld        = money.ErrCreditGrantHeld
)

// maxSourceIDBytes bounds a caller's source_id, as the tables holding it do.
const maxSourceIDBytes = 255

// CreateCreditGrants grants 1 to billing.MaxBatchItems customers prepaid
// credit, all or none, in one transaction; the answer is in request order.
// Each grant is idempotent on its customer and SourceID: an identical retry
// answers it Replayed, a changed one refuses the batch. Credit repays
// outstanding owed first.
func (s *Service) CreateCreditGrants(ctx context.Context, items []billing.CreateCreditGrantParams) ([]billing.CreditGrant, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if len(items) == 0 || len(items) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("items must hold 1 to %d grants", billing.MaxBatchItems).WithParam("items")
	}
	deposits := make([]money.DepositParams, len(items))
	seen := make(map[[2]string]bool, len(items))
	for i, item := range items {
		param := func(field string) string { return apperr.ItemParam(i, field) }
		if item.CustomerID.IsZero() {
			return nil, apperr.Invalidf("customer_id is required").WithParam(param("customer_id"))
		}
		if item.Amount <= 0 {
			return nil, apperr.Invalidf("amount must be positive").WithParam(param("amount"))
		}
		if strings.TrimSpace(item.Source) == "" {
			return nil, apperr.Invalidf("source is required").WithParam(param("source"))
		}
		if strings.TrimSpace(item.SourceID) == "" || len(item.SourceID) > maxSourceIDBytes {
			return nil, apperr.Invalidf("source_id must contain 1 to %d bytes", maxSourceIDBytes).WithParam(param("source_id"))
		}
		currency := moneyutil.NormalizeCurrency(strings.TrimSpace(item.Currency))
		if err := moneyutil.ValidateCurrency(currency); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam(param("currency")), currency)
		}
		key, err := money.NewIdempotencyKey(money.OpDeposit, item.Source, item.SourceID)
		if err != nil {
			return nil, apperr.Invalidf("%s", err.Error()).WithParam(param("source_id"))
		}
		sourceID := key.SourceID()
		if seen[[2]string{item.CustomerID.String(), sourceID}] {
			return nil, apperr.Invalidf("customer %s names source_id %q twice", item.CustomerID, item.SourceID).WithParam(param("source_id"))
		}
		seen[[2]string{item.CustomerID.String(), sourceID}] = true
		customer := identity.CustomerID(item.CustomerID)
		invoker := strings.TrimSpace(item.Invoker)
		if invoker == "" {
			invoker = customer.String()
		}
		var expiresAt *time.Time
		if item.ExpiresAt != nil {
			v := item.ExpiresAt.UTC()
			expiresAt = &v
		}
		deposits[i] = money.DepositParams{
			CustomerID: &customer, Invoker: invoker, Currency: currency, Amount: item.Amount,
			Source: key.Source(), SourceID: &sourceID, ExpiresAt: expiresAt, Description: item.Description,
			RepayOwed: true,
		}
	}
	trxs, err := s.moneyService().DepositBatch(ctx, deposits)
	var refused *money.DepositItemError
	if errors.As(err, &refused) && errors.Is(refused.Err, money.ErrIdempotencyKeyReused) {
		return nil, apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, refused.Err.Error()).WithParam(apperr.ItemParam(refused.Index, "source_id"))
	}
	if err != nil {
		return nil, err
	}
	out := make([]billing.CreditGrant, len(trxs))
	for i, trx := range trxs {
		grant, err := s.moneyService().GetCreditGrant(ctx, *deposits[i].CustomerID, trx.ID)
		if err != nil {
			return nil, err
		}
		grant.Replayed = trx.Replayed
		out[i] = *grant
	}
	return out, nil
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

// ListBalanceTransactions lists a customer's ledger in one currency, newest
// first.
func (s *Service) ListBalanceTransactions(ctx context.Context, customer identity.CustomerID, params billing.BalanceTransactionListParams) (billing.ListPage[billing.BalanceTransaction], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.BalanceTransaction]{}, err
	}
	defer release()
	if params.IDs == nil {
		currency, err := requireCurrency(params.Currency)
		if err != nil {
			return billing.ListPage[billing.BalanceTransaction]{}, err
		}
		params.Currency = currency
	}
	return s.moneyService().ListBalanceTransactions(ctx, customer, params)
}
