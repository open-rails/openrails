package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
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

// InvoiceLineItemDTO is one metered-usage line on an invoice: the per-event_type
// (per model/endpoint) total amount, event count, and summed dimensions. It
// mirrors models.InvoiceLineItem on the public facade so HTTP/library callers
// don't import the internal models/credits packages.
type InvoiceLineItemDTO struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

// InvoiceDTO is the public view of a finalized monthly itemized invoice (issue
// #303), served by the customer-facing GET /v1/self/invoices[/:id] routes. It is
// a public projection of models.Invoice so callers don't import internal types.
type InvoiceDTO struct {
	ID                uuid.UUID            `json:"id"`
	Currency          string               `json:"currency"`
	InvoiceNumber     *string              `json:"invoice_number,omitempty"`
	PeriodFrom        time.Time            `json:"period_from"`
	PeriodTo          time.Time            `json:"period_to"`
	UsageTotal        int64                `json:"usage_total"`
	DepositsTotal     int64                `json:"deposits_total"`
	OwedAccrued       int64                `json:"owed_accrued"`
	OwedPaid          int64                `json:"owed_paid"`
	ClosingBalance    int64                `json:"closing_balance"`
	SubtotalAmount    int64                `json:"subtotal_amount"`
	TotalAmount       int64                `json:"total_amount"`
	AmountPaid        int64                `json:"amount_paid"`
	AmountDue         int64                `json:"amount_due"`
	LineItems         []InvoiceLineItemDTO `json:"line_items"`
	MoneyMovements    map[string]int64     `json:"money_movements,omitempty"`
	Status            string               `json:"status"`
	CollectionMethod  string               `json:"collection_method"`
	IssuedAt          *time.Time           `json:"issued_at,omitempty"`
	DueAt             *time.Time           `json:"due_at,omitempty"`
	PaidAt            *time.Time           `json:"paid_at,omitempty"`
	VoidedAt          *time.Time           `json:"voided_at,omitempty"`
	UncollectibleAt   *time.Time           `json:"uncollectible_at,omitempty"`
	SentAt            *time.Time           `json:"sent_at,omitempty"`
	FinalizedAt       *time.Time           `json:"finalized_at,omitempty"`
	ExternalInvoiceID *string              `json:"external_invoice_id,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
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
		ID:                inv.ID,
		Currency:          inv.Currency,
		InvoiceNumber:     inv.InvoiceNumber,
		PeriodFrom:        inv.PeriodFrom,
		PeriodTo:          inv.PeriodTo,
		UsageTotal:        inv.UsageTotal,
		DepositsTotal:     inv.DepositsTotal,
		OwedAccrued:       inv.OwedAccrued,
		OwedPaid:          inv.OwedPaid,
		ClosingBalance:    inv.ClosingBalance,
		SubtotalAmount:    inv.SubtotalAmount,
		TotalAmount:       inv.TotalAmount,
		AmountPaid:        inv.AmountPaid,
		AmountDue:         inv.AmountDue,
		LineItems:         items,
		MoneyMovements:    inv.MoneyMovements,
		Status:            inv.Status,
		CollectionMethod:  inv.CollectionMethod,
		IssuedAt:          inv.IssuedAt,
		DueAt:             inv.DueAt,
		PaidAt:            inv.PaidAt,
		VoidedAt:          inv.VoidedAt,
		UncollectibleAt:   inv.UncollectibleAt,
		SentAt:            inv.SentAt,
		FinalizedAt:       inv.FinalizedAt,
		ExternalInvoiceID: inv.ExternalInvoiceID,
		CreatedAt:         inv.CreatedAt,
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

// AuthorizeSpendRequest is the input to AuthorizeSpend — the read+decision side
// of the authorize/hold route (issue #235). The hold itself is placed by
// HoldCredits once this allows it.
type AuthorizeSpendRequest struct {
	CustomerID      identity.CustomerID
	Invoker         string
	Currency        string
	EstimatedAmount int64
}

// AuthorizeSpendResult is the decision returned to the caller (mirrors the
// authorize route payload).
type AuthorizeSpendResult struct {
	Allowed               bool              `json:"allowed"`
	DenyCode              string            `json:"deny_code,omitempty"`
	BillingMode           string            `json:"billing_mode"`
	Currency              string            `json:"currency"`
	AvailableAmount       int64             `json:"available_amount"`
	OutstandingOwedAmount int64             `json:"outstanding_owed_amount"`
	RetryAfterSeconds     int64             `json:"retry_after_seconds,omitempty"`
	NextAllowedAt         *time.Time        `json:"next_allowed_at,omitempty"`
	Caps                  []money.CapResult `json:"caps,omitempty"`
}

// DenyInsufficientBalance is the deny code when a prepaid account lacks the
// available balance to cover the estimate.
const DenyInsufficientBalance = "insufficient_balance"

// AuthorizeSpend evaluates whether an estimated charge is permitted for an
// payer: the per-account/per-invoker spend policy (CheckSpendAllowed) AND, for
// prepaid accounts, available balance. It does NOT move money. (#235)
func (s *Service) AuthorizeSpend(ctx context.Context, req AuthorizeSpendRequest) (*AuthorizeSpendResult, error) {
	if req.EstimatedAmount < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	if req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
	}

	snap, err := s.GetCreditAccount(ctx, req.CustomerID, currency)
	if err != nil {
		return nil, err
	}
	dec, err := s.moneyService().CheckSpendAllowed(ctx, req.CustomerID, currency, strings.TrimSpace(req.Invoker), req.EstimatedAmount)
	if err != nil {
		return nil, err
	}

	res := &AuthorizeSpendResult{
		Allowed:               dec.Allowed,
		DenyCode:              dec.DenyCode,
		BillingMode:           snap.BillingMode,
		Currency:              snap.Currency,
		AvailableAmount:       snap.AvailableAmount,
		OutstandingOwedAmount: snap.OutstandingOwedAmount,
		RetryAfterSeconds:     dec.RetryAfterSeconds,
		NextAllowedAt:         dec.NextAllowedAt,
		Caps:                  dec.Caps,
	}

	// Prepaid accounts are additionally gated on available balance; arrears
	// accounts are gated by the outstanding ceiling inside CheckSpendAllowed.
	if snap.BillingMode != money.BillingModeArrears && req.EstimatedAmount > snap.AvailableAmount {
		res.Allowed = false
		if res.DenyCode == "" {
			res.DenyCode = DenyInsufficientBalance
		}
	}
	return res, nil
}

// AuthorizeAndHoldRequest is the input to AuthorizeAndHold — the atomic
// capacity-decision + hold placement that backs POST /v1/service/credits/authorize
// (issue #235/#247). Payer is the merchant subject billed; RequestID is the
// idempotency key for the placed hold.
type AuthorizeAndHoldRequest struct {
	CustomerID      identity.CustomerID
	Invoker         string
	InvokerType     string
	Currency        string
	EstimatedAmount int64
	RequestID       string
	Resource        string
	// ExpiresAt bounds the placed hold; when zero a default TTL is applied.
	ExpiresAt time.Time
}

// AuthorizeAndHoldResult is the account-capacity decision and start-capacity
// snapshot.
// In-flight holds are request-id keyed Redis state in the #505 model, so no
// Postgres reservation id is returned here.
type AuthorizeAndHoldResult struct {
	Allowed               bool       `json:"allowed"`
	DenyCode              string     `json:"deny_code,omitempty"`
	BillingMode           string     `json:"billing_mode"`
	Currency              string     `json:"currency"`
	AvailableAmount       int64      `json:"available_amount"`
	OutstandingOwedAmount int64      `json:"outstanding_owed_amount"`
	StartCapacityAmount   int64      `json:"start_capacity_amount"`
	HoldExpiresAt         *time.Time `json:"hold_expires_at,omitempty"`
}

// authorizeHoldSource is the source label recorded on holds placed by the
// authorize route, so they are attributable + idempotency-keyed by request_id.
const authorizeHoldSource = "authorize"

// defaultAuthorizeHoldTTL bounds a placed hold when the caller does not supply an
// explicit expiry. A hold is a short-lived reservation against an in-flight
// invocation; the reconcile/orphan-hold sweeper (#243) is the backstop.
const defaultAuthorizeHoldTTL = 15 * time.Minute

// AuthorizeAndHold runs the prepaid/arrears capacity gate under the per-customer
// money lock, then places the Redis request hold. The actual in-flight
// reservation lives outside the durable money ledger in the #505 Redis hold
// model.
func (s *Service) AuthorizeAndHold(ctx context.Context, req AuthorizeAndHoldRequest) (*AuthorizeAndHoldResult, error) {
	if req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if req.EstimatedAmount < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	req.InvokerType = strings.TrimSpace(req.InvokerType)
	if req.EstimatedAmount > 0 && req.InvokerType != string(identity.InvokerTypePayer) && req.InvokerType != string(identity.InvokerTypeDelegated) {
		return nil, fmt.Errorf("invoker_type must be payer or delegated")
	}
	expires := req.ExpiresAt
	if expires.IsZero() {
		expires = s.now().Add(defaultAuthorizeHoldTTL).UTC()
	}

	out, err := s.moneyService().AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer:           req.CustomerID,
		Invoker:         strings.TrimSpace(req.Invoker),
		Currency:        currency,
		EstimatedAmount: req.EstimatedAmount,
		Source:          authorizeHoldSource,
		SourceID:        req.RequestID,
		ExpiresAt:       expires,
	})
	if err != nil {
		return nil, err
	}

	res := &AuthorizeAndHoldResult{
		Allowed:               out.Decision.Allowed,
		DenyCode:              out.Decision.DenyCode,
		BillingMode:           out.BillingMode,
		Currency:              out.Currency,
		AvailableAmount:       out.AvailableAmount,
		OutstandingOwedAmount: out.OutstandingOwedAmount,
		StartCapacityAmount:   out.AccountCapacityAmount,
	}
	if out.Decision.Allowed && req.EstimatedAmount > 0 {
		store, err := s.holdStore()
		if err != nil {
			return nil, err
		}
		mid, err := serviceMerchantID(ctx)
		if err != nil {
			return nil, err
		}
		allowed, activeHeld, err := store.Place(ctx, holds.Hold{
			MerchantID:      mid,
			RequestID:       req.RequestID,
			CustomerID:      req.CustomerID.UUID().String(),
			Invoker:         strings.TrimSpace(req.Invoker),
			InvokerType:     req.InvokerType,
			Currency:        currency,
			EstimatedAmount: req.EstimatedAmount,
			Source:          authorizeHoldSource,
			Resource:        strings.TrimSpace(req.Resource),
			CreatedAt:       s.now().UTC(),
			ExpiresAt:       expires,
		}, out.AccountCapacityAmount)
		if err != nil {
			return nil, err
		}
		if !allowed {
			res.Allowed = false
			if res.DenyCode == "" {
				res.DenyCode = money.DenyInsufficientBalance
			}
			res.StartCapacityAmount = startCapacity(out.AccountCapacityAmount, activeHeld)
			return res, nil
		}
		res.StartCapacityAmount = startCapacity(out.AccountCapacityAmount, activeHeld-req.EstimatedAmount)
		res.HoldExpiresAt = &expires
	}
	return res, nil
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
// org billing-account admin surface (issue #242). RLS-scoped.
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

// SetSpendLimit upserts a per-invoker spend cap under an payer (issue #237).
func (s *Service) SetSpendLimit(ctx context.Context, payer identity.CustomerID, currency, invoker string, maxDay, maxMonth *int64) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	_, err := s.moneyService().SetSpendLimit(ctx, payer, currency, invoker, maxDay, maxMonth)
	return err
}
