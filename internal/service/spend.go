package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
)

var (
	ErrInvoiceNotRetryable             = money.ErrInvoiceNotRetryable
	ErrInvoiceRetryInProgress          = money.ErrInvoiceRetryInProgress
	ErrInvoiceRetryOutcomeUnknown      = money.ErrInvoiceRetryOutcomeUnknown
	ErrInvoiceRetryIdempotencyConflict = money.ErrInvoiceRetryIdempotencyConflict
)

// GetBalance returns a customer's money in one currency.
func (s *Service) GetBalance(ctx context.Context, customer identity.CustomerID, currency string) (*billing.Balance, error) {
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	var out *billing.Balance
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		bal, err := s.moneyService().GetBalanceForCustomer(ctx, customer, currency)
		if err != nil {
			return err
		}
		settings, err := s.moneyService().GetAccountSettings(ctx, customer, currency)
		if err != nil {
			return err
		}
		owed, err := s.moneyService().GetOutstandingOwed(ctx, customer, currency)
		if err != nil {
			return err
		}
		out = &billing.Balance{
			CustomerID:      customer,
			Currency:        currency,
			BillingMode:     billing.BillingMode(settings.BillingMode),
			BalanceAmount:   bal.Balance,
			HeldAmount:      bal.HeldBalance,
			AvailableAmount: bal.Balance - bal.HeldBalance,
			OwedAmount:      owed,
		}
		return nil
	})
	return out, err
}

// GetUsage reports a customer's usage in one currency over [From, To),
// grouped by GroupBy (event_type when empty).
func (s *Service) GetUsage(ctx context.Context, customer identity.CustomerID, params billing.GetUsageParams) (*billing.Usage, error) {
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if params.From.IsZero() || params.To.IsZero() || !params.To.After(params.From) {
		return nil, fmt.Errorf("from and to must bound a window")
	}
	if params.GroupBy == "" {
		params.GroupBy = billing.UsageByEventType
	}
	out := &billing.Usage{CustomerID: customer, Currency: currency, From: params.From.UTC(), To: params.To.UTC(), GroupBy: params.GroupBy, Rows: []billing.UsageRow{}}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if params.GroupBy == billing.UsageByEventType {
			rows, err := s.moneyService().AggregateUsage(ctx, customer, currency, out.From, out.To)
			if err != nil {
				return err
			}
			for _, r := range rows {
				out.Rows = append(out.Rows, billing.UsageRow{Key: r.EventType, EventCount: r.EventCount, Amount: r.TotalAmount, Dimensions: r.Dimensions})
			}
			sort.Slice(out.Rows, func(i, j int) bool {
				if out.Rows[i].Amount != out.Rows[j].Amount {
					return out.Rows[i].Amount > out.Rows[j].Amount
				}
				return out.Rows[i].Key < out.Rows[j].Key
			})
			return nil
		}
		rows, err := s.moneyService().ServiceUsageRollup(ctx, customer, currency, out.From, out.To, string(params.GroupBy))
		if err != nil {
			return err
		}
		for _, r := range rows {
			out.Rows = append(out.Rows, billing.UsageRow{Key: r.Key, EventCount: r.EventCount, Amount: r.TotalAmount})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetCreditAccountSettings upserts an payer's spend policy (issue #237/#235
// admin surface). Thin passthrough to the credits service.
func (s *Service) SetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string, in money.AccountSettingsInput) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	// Pin a merchant connection for the upsert (#227).
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.moneyService().UpsertAccountSettings(ctx, payer, currency, in)
		return err
	})
}

// GetCreditAccountSettings returns a payer's stored account settings
// (billing mode, credit limit and collection method) for the
// customer billing-account admin surface (issue #242). Merchant-scoped.
func (s *Service) GetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	return s.moneyService().GetAccountSettingsForCustomer(ctx, payer, currency)
}
