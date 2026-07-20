package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

var (
	ErrInvoiceNotRetryable             = money.ErrInvoiceNotRetryable
	ErrCollectionPaymentMethodRequired = money.ErrCollectionPaymentMethodRequired
	ErrCollectionPaymentMethodInvalid  = money.ErrCollectionPaymentMethodInvalid
	ErrInvoiceRetryInProgress          = money.ErrInvoiceRetryInProgress
	ErrInvoiceRetryOutcomeUnknown      = money.ErrInvoiceRetryOutcomeUnknown
	ErrInvoiceRetryIdempotencyConflict = money.ErrInvoiceRetryIdempotencyConflict
)

// CreditAccountSnapshot is the balance + policy view of an payer's credit
// account, served by the authorize/balance route (issue #235).
type CreditAccountSnapshot struct {
	CustomerID            uuid.UUID `json:"customer_id"`
	Currency              string    `json:"currency"`
	BillingMode           string    `json:"billing_mode"`
	BalanceAmount         int64     `json:"balance_amount"`
	HeldAmount            int64     `json:"held_amount"`
	AvailableAmount       int64     `json:"available_amount"`
	OutstandingOwedAmount int64     `json:"outstanding_owed_amount"`
}

// GetCreditAccount returns the balance + policy snapshot for an merchant subject.
func (s *Service) GetCreditAccount(ctx context.Context, payer identity.CustomerID, currency string) (*CreditAccountSnapshot, error) {
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	// Pin a merchant-scoped connection so the balance/settings reads set the RLS GUC
	// and are merchant-scoped under the openrails_app role (#227). RunInMerchantConn
	// reuses the request's already-pinned connection when the HTTP middleware set
	// one, so this is safe whether called from a request or directly.
	var snap *CreditAccountSnapshot
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		bal, err := s.moneyService().GetBalanceForCustomer(ctx, payer, currency)
		if err != nil {
			return err
		}
		if money.IsQualifiedUnit(currency) {
			snap = &CreditAccountSnapshot{
				CustomerID:            payer.UUID(),
				Currency:              currency,
				BillingMode:           money.BillingModePrepaid,
				BalanceAmount:         bal.Balance,
				HeldAmount:            bal.HeldBalance,
				AvailableAmount:       bal.Balance - bal.HeldBalance,
				OutstandingOwedAmount: 0,
			}
			return nil
		}
		settings, err := s.moneyService().GetAccountSettings(ctx, payer, currency)
		if err != nil {
			return err
		}
		outstanding, err := s.moneyService().GetOutstandingOwed(ctx, payer, currency)
		if err != nil {
			return err
		}
		snap = &CreditAccountSnapshot{
			CustomerID:            payer.UUID(),
			Currency:              currency,
			BillingMode:           settings.BillingMode,
			BalanceAmount:         bal.Balance,
			HeldAmount:            bal.HeldBalance,
			AvailableAmount:       bal.Balance - bal.HeldBalance,
			OutstandingOwedAmount: outstanding,
		}
		return nil
	})
	return snap, err
}

// UsageRow is one per-event_type usage rollup over a window: summed host-priced
// amount, event count, and summed per-dimension counts (issue #289). It mirrors
// money.UsageRollupRow but lives on the public facade so HTTP/library callers
// don't import the internal credits package.
type UsageRow struct {
	EventType   string           `json:"event_type"`
	Currency    string           `json:"currency"`
	TotalAmount int64            `json:"total_amount"`
	EventCount  int64            `json:"event_count"`
	Dimensions  map[string]int64 `json:"dimensions"`
}

// GetUsage rolls up an payer's usage events over [from, to) grouped by
// event_type with summed dimensions (issue #289). Like GetCreditAccount it pins
// a merchant-scoped connection so the rollup query runs RLS-scoped under the
// openrails_app role (#227); RunInMerchantConn reuses the request's already-pinned
// connection when one is set.
func (s *Service) GetUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) ([]UsageRow, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out []UsageRow
	err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.moneyService().AggregateUsage(ctx, payer, currency, from, to)
		if err != nil {
			return err
		}
		out = make([]UsageRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, UsageRow{
				EventType:   r.EventType,
				Currency:    r.Currency,
				TotalAmount: r.TotalAmount,
				EventCount:  r.EventCount,
				Dimensions:  r.Dimensions,
			})
		}
		return nil
	})
	return out, err
}

// InvoiceLineItemDTO is one statement line on an invoice: a per-event_type usage
// rollup (total amount, event count, summed dimensions) or an adjustment line
// (event_type = kind, e.g. "minimum_spend_trueup"). It mirrors
// models.InvoiceLineItem on the public facade so HTTP/library callers don't
// import the internal models/credits packages.
type InvoiceLineItemDTO struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

// InvoiceDTO is the public view of a finalized monthly itemized invoice (issue
// #303), served by the customer-facing GET /v1/me/invoices[/:id] routes. It is
// a public projection of models.Invoice so callers don't import internal types.
type InvoiceDTO struct {
	ID                        uuid.UUID            `json:"id"`
	Currency                  string               `json:"currency"`
	InvoiceNumber             *string              `json:"invoice_number,omitempty"`
	PeriodFrom                time.Time            `json:"period_from"`
	PeriodTo                  time.Time            `json:"period_to"`
	UsageTotal                int64                `json:"usage_total"`
	DepositsTotal             int64                `json:"deposits_total"`
	OwedAccrued               int64                `json:"owed_accrued"`
	OwedPaid                  int64                `json:"owed_paid"`
	ClosingBalance            int64                `json:"closing_balance"`
	SubtotalAmount            int64                `json:"subtotal_amount"`
	TotalAmount               int64                `json:"total_amount"`
	AmountPaid                int64                `json:"amount_paid"`
	AmountDue                 int64                `json:"amount_due"`
	LineItems                 []InvoiceLineItemDTO `json:"line_items"`
	MoneyMovements            map[string]int64     `json:"money_movements,omitempty"`
	PONumber                  *string              `json:"po_number,omitempty"`
	Tax                       map[string]any       `json:"tax,omitempty"`
	BillingContacts           []InvoiceContactDTO  `json:"billing_contacts,omitempty"`
	Memo                      *string              `json:"memo,omitempty"`
	Status                    string               `json:"status"`
	CollectionMethod          string               `json:"collection_method"`
	IssuedAt                  *time.Time           `json:"issued_at,omitempty"`
	DueAt                     *time.Time           `json:"due_at,omitempty"`
	PaidAt                    *time.Time           `json:"paid_at,omitempty"`
	VoidedAt                  *time.Time           `json:"voided_at,omitempty"`
	UncollectibleAt           *time.Time           `json:"uncollectible_at,omitempty"`
	FinalizedAt               *time.Time           `json:"finalized_at,omitempty"`
	ExternalInvoiceID         *string              `json:"external_invoice_id,omitempty"`
	CollectionFailureCount    int32                `json:"collection_failure_count"`
	CollectionFailedAt        *time.Time           `json:"collection_failed_at,omitempty"`
	NextCollectionAttemptAt   *time.Time           `json:"next_collection_attempt_at,omitempty"`
	LastCollectionFailureCode *string              `json:"last_collection_failure_code,omitempty"`
	CreatedAt                 time.Time            `json:"created_at"`
}

// InvoicePaymentAttemptDTO is one automatic collection attempt for an invoice.
type InvoicePaymentAttemptDTO struct {
	ID              uuid.UUID  `json:"id"`
	InvoiceID       uuid.UUID  `json:"invoice_id"`
	Currency        string     `json:"currency"`
	Amount          int64      `json:"amount"`
	Status          string     `json:"status"`
	PaymentMethodID *uuid.UUID `json:"payment_method_id,omitempty"`
	Rail            *string    `json:"rail,omitempty"`
	RailPaymentID   *string    `json:"rail_payment_id,omitempty"`
	FailureCode     *string    `json:"failure_code,omitempty"`
	FailureReason   *string    `json:"failure_reason,omitempty"`
	AttemptedAt     time.Time  `json:"attempted_at"`
	SettledAt       *time.Time `json:"settled_at,omitempty"`
}

type InvoiceCollectionRetryRequest struct {
	InvoiceID       uuid.UUID `json:"invoice_id"`
	IdempotencyKey  string    `json:"-"`
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
}

type InvoiceCollectionRetryResult struct {
	Invoice  InvoiceDTO               `json:"invoice"`
	Attempt  InvoicePaymentAttemptDTO `json:"attempt"`
	Replayed bool                     `json:"replayed"`
}

// InvoiceContactDTO is one billing contact on an invoice document (#798).
type InvoiceContactDTO struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

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
		PaymentMethodID: attempt.PaymentMethodID,
		Rail:            attempt.Rail,
		RailPaymentID:   attempt.RailPaymentID,
		FailureCode:     attempt.FailureCode,
		FailureReason:   attempt.FailureReason,
		AttemptedAt:     attempt.AttemptedAt,
		SettledAt:       attempt.SettledAt,
	}
}

// ListInvoices lists an payer's finalized invoices, newest period first,
// paginated (issue #303). Like GetUsage it pins a merchant-scoped connection so the
// read runs RLS-scoped under the openrails_app role (#227); RunInMerchantConn
// reuses the request's already-pinned connection when one is set. Returns the
// page of public DTOs plus the total count for pagination.
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

// GetInvoice returns one finalized invoice (with its line items) for an payer by
// id (issue #303). RLS-scoped like ListInvoices; an invoice belonging to another
// payer/merchant is unreachable (fail closed). Returns a public DTO.
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

// RetryInvoiceCollection reopens a failed automatic-collection invoice and
// immediately retries it through the configured runtime collection plane.
func (s *Service) RetryInvoiceCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID) (*InvoiceDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.MoneyCharger == nil {
		return nil, fmt.Errorf("invoice collection charger not configured")
	}
	var out *InvoiceDTO
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		invoice, err := s.moneyService().RetryInvoiceCollection(ctx, rt.MoneyCharger, payer, invoiceID)
		if err != nil {
			return err
		}
		dto := invoiceToDTO(invoice)
		out = &dto
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RetryInvoiceCollectionIdempotent retries collection with a durable client
// key and an explicitly bound payer-owned saved method.
func (s *Service) RetryInvoiceCollectionIdempotent(ctx context.Context, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.MoneyCharger == nil {
		return nil, fmt.Errorf("invoice collection charger not configured")
	}
	var out *InvoiceCollectionRetryResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().RetryInvoiceCollectionIdempotent(ctx, rt.MoneyCharger, payer, money.InvoiceCollectionRetryRequest{
			InvoiceID: request.InvoiceID, IdempotencyKey: request.IdempotencyKey, PaymentMethodID: request.PaymentMethodID,
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
	// Pin a merchant connection so the upsert sets the RLS GUC under openrails_app (#227).
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.moneyService().UpsertAccountSettings(ctx, payer, currency, in)
		return err
	})
}

// SetCreditLimit sets the admin/operator arrears credit line for a payer (#489):
// under billing_mode=arrears the balance may go negative up to creditLimit;
// AdmitHold denies insufficient_credit when a new hold would exceed remaining
// capacity. 0 means no arrears capacity; prepaid balance may still be spent.
// OPERATOR-only — deliberately separate from SetCreditAccountSettings (the
// self-serve surface): a subject cannot raise its own credit line.
func (s *Service) SetCreditLimit(ctx context.Context, payer identity.CustomerID, currency string, creditLimit int64) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.moneyService().SetCreditLimit(ctx, payer, currency, creditLimit)
	})
}

// GetCreditLimit returns the admin-set arrears credit line for a payer (#489).
func (s *Service) GetCreditLimit(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	if s == nil || s.rt == nil {
		return 0, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return 0, fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return 0, err
	}
	return s.moneyService().GetCreditLimit(ctx, payer, currency)
}

// GetCreditAccountSettings returns a payer's stored account settings
// (billing mode prepaid|arrears, spend caps, auto-top-up, expiry default) for the
// customer billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	return s.moneyService().GetAccountSettingsForCustomer(ctx, payer, currency)
}

// GetCustomerCreditTransactions lists a merchant subject's money transactions for
// the billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetCustomerCreditTransactions(ctx context.Context, payer identity.CustomerID, currency string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, 0, err
	}
	var items []models.MoneyTransaction
	var total int
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		items, total, e = s.moneyService().GetTransactionsByCustomer(ctx, payer, currency, limit, offset)
		return e
	})
	return items, total, err
}
