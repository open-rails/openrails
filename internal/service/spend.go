package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var (
	ErrInvoiceNotRetryable             = money.ErrInvoiceNotRetryable
	ErrCollectionPaymentMethodRequired = money.ErrCollectionPaymentMethodRequired
	ErrCollectionPaymentMethodInvalid  = money.ErrCollectionPaymentMethodInvalid
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
func (s *Service) GetUsage(ctx context.Context, customer identity.CustomerID, params billing.UsageParams) (*billing.Usage, error) {
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

// InvoiceLineItemDTO is one statement line on an invoice: a per-event_type usage
// rollup (total amount, event count, summed dimensions) or an adjustment line
// (event_type identifies the billed usage). It mirrors
// models.InvoiceLineItem on the public facade so HTTP/library callers don't
// import the internal models/credits packages.
type InvoiceLineItemDTO = billing.InvoiceLineItemDTO

// InvoiceDTO is the public view of a finalized monthly itemized invoice (issue
// #303), served by the customer-facing GET /v1/me/invoices[/:id] routes. It is
// a public projection of models.Invoice so callers don't import internal types.
type InvoiceDTO = billing.InvoiceDTO

// InvoicePaymentAttemptDTO is one automatic collection attempt for an invoice.
type InvoicePaymentAttemptDTO = billing.InvoicePaymentAttemptDTO

type InvoiceCollectionRetryRequest = billing.InvoiceCollectionRetryRequest

type InvoiceCollectionRetryResult = billing.InvoiceCollectionRetryResult

// InvoiceContactDTO is one billing contact on an invoice document (#798).
type InvoiceContactDTO = billing.InvoiceContactDTO

func contactsToDTO(contacts []models.InvoiceContact) []InvoiceContactDTO {
	if len(contacts) == 0 {
		return nil
	}
	out := make([]InvoiceContactDTO, 0, len(contacts))
	for _, c := range contacts {
		out = append(out, InvoiceContactDTO{Name: c.Name, Email: c.Email})
	}
	return out
}

// invoiceToDTO projects an internal models.Invoice onto the public InvoiceDTO.
func invoiceToDTO(inv *models.Invoice) InvoiceDTO {
	items := make([]InvoiceLineItemDTO, 0, len(inv.LineItems))
	for _, li := range inv.LineItems {
		items = append(items, InvoiceLineItemDTO{
			EventType:  li.EventType,
			Amount:     li.Amount,
			Count:      li.Count,
			Dimensions: li.Dimensions,
		})
	}
	return InvoiceDTO{
		ID:                        inv.ID,
		Currency:                  inv.Currency,
		InvoiceNumber:             inv.InvoiceNumber,
		PeriodFrom:                inv.PeriodFrom,
		PeriodTo:                  inv.PeriodTo,
		UsageTotal:                inv.UsageTotal,
		DepositsTotal:             inv.DepositsTotal,
		OwedAccrued:               inv.OwedAccrued,
		OwedPaid:                  inv.OwedPaid,
		ClosingBalance:            inv.ClosingBalance,
		SubtotalAmount:            inv.SubtotalAmount,
		TotalAmount:               inv.TotalAmount,
		AmountPaid:                inv.AmountPaid,
		AmountDue:                 inv.AmountDue,
		LineItems:                 items,
		MoneyMovements:            inv.MoneyMovements,
		PONumber:                  inv.PONumber,
		Tax:                       inv.Tax,
		BillingContacts:           contactsToDTO(inv.BillingContacts),
		Memo:                      inv.Memo,
		Status:                    inv.Status,
		CollectionMethod:          inv.CollectionMethod,
		IssuedAt:                  inv.IssuedAt,
		DueAt:                     inv.DueAt,
		PaidAt:                    inv.PaidAt,
		VoidedAt:                  inv.VoidedAt,
		UncollectibleAt:           inv.UncollectibleAt,
		FinalizedAt:               inv.FinalizedAt,
		ExternalInvoiceID:         inv.ExternalInvoiceID,
		CollectionFailureCount:    inv.CollectionFailureCount,
		CollectionFailedAt:        inv.CollectionFailedAt,
		NextCollectionAttemptAt:   inv.NextCollectionAttemptAt,
		LastCollectionFailureCode: inv.LastCollectionFailureCode,
		CollectionIntentID:        inv.CollectionIntentID,
		CreatedAt:                 inv.CreatedAt,
	}
}

func invoicePaymentAttemptToDTO(attempt models.InvoicePaymentAttempt) InvoicePaymentAttemptDTO {
	return InvoicePaymentAttemptDTO{
		ID:              attempt.ID,
		InvoiceID:       attempt.InvoiceID,
		Currency:        attempt.Currency,
		Amount:          attempt.Amount,
		Status:          attempt.Status,
		PaymentMethodID: (*billing.PaymentMethodID)(attempt.PaymentMethodID),
		Rail:            attempt.Rail,
		RailPaymentID:   attempt.RailPaymentID,
		FailureCode:     attempt.FailureCode,
		FailureReason:   attempt.FailureReason,
		AttemptedAt:     attempt.AttemptedAt,
		SettledAt:       attempt.SettledAt,
	}
}

// ListInvoices lists an payer's finalized invoices, newest period first,
// paginated (issue #303). Like GetUsage it pins a merchant-scoped connection so
// the read runs on the merchant's session (#227); RunInMerchantConn reuses the
// request's already-pinned connection when one is set. Returns the page of
// public DTOs plus the total count for pagination.
func (s *Service) ListInvoices(ctx context.Context, payer identity.CustomerID, limit, offset int) ([]InvoiceDTO, int, error) {
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	var out []InvoiceDTO
	var total int
	err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, t, err := s.moneyService().ListInvoices(ctx, payer, limit, offset)
		if err != nil {
			return err
		}
		total = t
		out = make([]InvoiceDTO, 0, len(rows))
		for i := range rows {
			out = append(out, invoiceToDTO(&rows[i]))
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetInvoice returns one finalized invoice (with its line items) for an payer
// by id (issue #303). Merchant-scoped like ListInvoices; an invoice belonging
// to another payer/merchant is unreachable (fail closed). Returns a public DTO.
func (s *Service) GetInvoice(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*InvoiceDTO, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out *InvoiceDTO
	err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		inv, err := s.moneyService().GetInvoiceByID(ctx, payer, id)
		if err != nil {
			return err
		}
		dto := invoiceToDTO(inv)
		out = &dto
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListInvoicePaymentAttempts returns one payer-owned invoice's collection
// history, newest attempt first, plus the total count for pagination.
func (s *Service) ListInvoicePaymentAttempts(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, limit, offset int) ([]InvoicePaymentAttemptDTO, int, error) {
	if s == nil || s.rt == nil {
		return nil, 0, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	var out []InvoicePaymentAttemptDTO
	var total int
	err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		attempts, count, err := s.moneyService().ListInvoicePaymentAttempts(ctx, payer, invoiceID, limit, offset)
		if err != nil {
			return err
		}
		total = count
		out = make([]InvoicePaymentAttemptDTO, 0, len(attempts))
		for _, attempt := range attempts {
			out = append(out, invoicePaymentAttemptToDTO(attempt))
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// SetInvoiceCollectionPaymentMethod selects the payer-owned saved method used
// for automatic invoice collection in one billing currency.
func (s *Service) SetInvoiceCollectionPaymentMethod(ctx context.Context, payer identity.CustomerID, currency string, paymentMethodID uuid.UUID) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.moneyService().SetInvoiceCollectionPaymentMethod(ctx, payer, currency, paymentMethodID)
	})
}

// RetryInvoiceCollectionIdempotent retries collection with a durable client
// key and an explicitly bound payer-owned saved method through the
// invoice_collection intent machine.
func (s *Service) RetryInvoiceCollectionIdempotent(ctx context.Context, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.MoneyCharger == nil {
		return nil, fmt.Errorf("invoice collection charger not configured")
	}
	var out *InvoiceCollectionRetryResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().RetryInvoiceCollection(ctx, rt.IntentRunner(), payer, money.InvoiceCollectionRetryRequest{
			InvoiceID: request.InvoiceID, IdempotencyKey: request.IdempotencyKey, PaymentMethodID: request.PaymentMethodID.UUID(),
		})
		if err != nil {
			return err
		}
		out = &InvoiceCollectionRetryResult{
			Invoice: invoiceToDTO(result.Invoice), Attempt: invoicePaymentAttemptToDTO(result.Attempt), Replayed: result.Replayed,
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

// SetCreditLimit sets how much a customer may owe in arrears in one
// currency. Merchant-only: a customer cannot raise its own credit line.
func (s *Service) SetCreditLimit(ctx context.Context, customer identity.CustomerID, params billing.CreditLimitParams) (*billing.CreditLimit, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	if params.Amount < 0 {
		return nil, apperr.Invalidf("amount must be nonnegative").WithParam("amount")
	}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.moneyService().SetCreditLimit(ctx, customer, currency, params.Amount)
	})
	if err != nil {
		return nil, err
	}
	return &billing.CreditLimit{CustomerID: customer, Currency: currency, Amount: params.Amount}, nil
}

// GetCreditLimit returns how much a customer may owe in arrears.
func (s *Service) GetCreditLimit(ctx context.Context, customer identity.CustomerID, currency string) (*billing.CreditLimit, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	amount, err := s.moneyService().GetCreditLimit(ctx, customer, currency)
	if err != nil {
		return nil, err
	}
	return &billing.CreditLimit{CustomerID: customer, Currency: currency, Amount: amount}, nil
}

// GetCustomerTrustLevel returns the trust level stored for a customer.
func (s *Service) GetCustomerTrustLevel(ctx context.Context, customer identity.CustomerID, currency string) (*billing.TrustLevel, error) {
	level, err := s.GetTrustLevel(ctx, customer, currency)
	if err != nil {
		return nil, err
	}
	return &billing.TrustLevel{CustomerID: customer, Currency: money.NormalizeCurrency(currency), TrustLevel: level}, nil
}

// SetTrustLevel stores the trust level a customer's admissions use when a
// request names none; empty clears it.
func (s *Service) SetTrustLevel(ctx context.Context, customer identity.CustomerID, params billing.TrustLevelParams) (*billing.TrustLevel, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	level := strings.TrimSpace(params.TrustLevel)
	if len(level) > 64 {
		return nil, apperr.Invalidf("trust_level exceeds 64 bytes").WithParam("trust_level")
	}
	if err := s.moneyService().SetTrustLevelOverride(ctx, customer, currency, level); err != nil {
		return nil, err
	}
	return &billing.TrustLevel{CustomerID: customer, Currency: currency, TrustLevel: level}, nil
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
