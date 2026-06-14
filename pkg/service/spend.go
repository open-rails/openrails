package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// CreditAccountSnapshot is the balance + policy view of an payer's credit
// account, served by the authorize/balance route (issue #235).
type CreditAccountSnapshot struct {
	CustomerID            uuid.UUID `json:"customer_id"`
	Currency              string    `json:"currency"`
	BillingMode           string    `json:"billing_mode"`
	BalanceMicros         int64     `json:"balance_micros"`
	HeldMicros            int64     `json:"held_micros"`
	AvailableMicros       int64     `json:"available_micros"`
	OutstandingOwedMicros int64     `json:"outstanding_owed_micros"`
}

// GetCreditAccount returns the balance + policy snapshot for an tenant subject.
func (s *Service) GetCreditAccount(ctx context.Context, payer identity.CustomerID, currency string) (*CreditAccountSnapshot, error) {
	// #476: the currency is echoed back on the snapshot; empty defaults to "USD".
	currency = strings.TrimSpace(currency)
	if currency == "" {
		currency = money.DefaultCurrency
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	// Pin a tenant-scoped connection so the balance/settings reads set the RLS GUC
	// and are tenant-scoped under the openrails_app role (#227). RunInTenantConn
	// reuses the request's already-pinned connection when the HTTP middleware set
	// one, so this is safe whether called from a request or directly.
	var snap *CreditAccountSnapshot
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		bal, err := s.moneyService().GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
		if err != nil {
			return err
		}
		settings, err := s.moneyService().GetAccountSettings(ctx, payer)
		if err != nil {
			return err
		}
		snap = &CreditAccountSnapshot{
			CustomerID:            payer.UUID(),
			Currency:              currency,
			BillingMode:           settings.BillingMode,
			BalanceMicros:         bal.Balance,
			HeldMicros:            bal.HeldBalance,
			AvailableMicros:       bal.Balance - bal.HeldBalance,
			OutstandingOwedMicros: settings.OutstandingOwedMicros,
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
	TotalAmount int64            `json:"total_amount"`
	EventCount  int64            `json:"event_count"`
	Dimensions  map[string]int64 `json:"dimensions"`
}

// GetUsage rolls up an payer's usage events over [from, to) grouped by
// event_type with summed dimensions (issue #289). Like GetCreditAccount it pins
// a tenant-scoped connection so the rollup query runs RLS-scoped under the
// openrails_app role (#227); RunInTenantConn reuses the request's already-pinned
// connection when one is set.
func (s *Service) GetUsage(ctx context.Context, payer identity.CustomerID, from, to time.Time) ([]UsageRow, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out []UsageRow
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, err := s.moneyService().AggregateUsage(ctx, payer, from, to)
		if err != nil {
			return err
		}
		out = make([]UsageRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, UsageRow{
				EventType:   r.EventType,
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
	ID             uuid.UUID            `json:"id"`
	Currency       string               `json:"currency"`
	PeriodFrom     time.Time            `json:"period_from"`
	PeriodTo       time.Time            `json:"period_to"`
	UsageTotal     int64                `json:"usage_total"`
	DepositsTotal  int64                `json:"deposits_total"`
	OwedAccrued    int64                `json:"owed_accrued"`
	OwedPaid       int64                `json:"owed_paid"`
	ClosingBalance int64                `json:"closing_balance"`
	LineItems      []InvoiceLineItemDTO `json:"line_items"`
	MoneyMovements map[string]int64     `json:"money_movements,omitempty"`
	Status         string               `json:"status"`
	FinalizedAt    *time.Time           `json:"finalized_at,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
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
		ID:             inv.ID,
		Currency:       inv.Currency,
		PeriodFrom:     inv.PeriodFrom,
		PeriodTo:       inv.PeriodTo,
		UsageTotal:     inv.UsageTotal,
		DepositsTotal:  inv.DepositsTotal,
		OwedAccrued:    inv.OwedAccrued,
		OwedPaid:       inv.OwedPaid,
		ClosingBalance: inv.ClosingBalance,
		LineItems:      items,
		MoneyMovements: inv.MoneyMovements,
		Status:         inv.Status,
		FinalizedAt:    inv.FinalizedAt,
		CreatedAt:      inv.CreatedAt,
	}
}

// ListInvoices lists an payer's finalized invoices, newest period first,
// paginated (issue #303). Like GetUsage it pins a tenant-scoped connection so the
// read runs RLS-scoped under the openrails_app role (#227); RunInTenantConn
// reuses the request's already-pinned connection when one is set. Returns the
// page of public DTOs plus the total count for pagination.
func (s *Service) ListInvoices(ctx context.Context, payer identity.CustomerID, limit, offset int) ([]InvoiceDTO, int, error) {
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	var out []InvoiceDTO
	var total int
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
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
// payer/tenant is unreachable (fail closed). Returns a public DTO.
func (s *Service) GetInvoice(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*InvoiceDTO, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out *InvoiceDTO
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
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
	CustomerID     identity.CustomerID
	Actor          string
	EstimateMicros int64
}

// AuthorizeSpendResult is the decision returned to the caller (mirrors the
// authorize route payload).
type AuthorizeSpendResult struct {
	Allowed               bool              `json:"allowed"`
	DenyCode              string            `json:"deny_code,omitempty"`
	BillingMode           string            `json:"billing_mode"`
	AvailableMicros       int64             `json:"available_micros"`
	OutstandingOwedMicros int64             `json:"outstanding_owed_micros"`
	RetryAfterSeconds     int64             `json:"retry_after_seconds,omitempty"`
	NextAllowedAt         *time.Time        `json:"next_allowed_at,omitempty"`
	Caps                  []money.CapResult `json:"caps,omitempty"`
}

// DenyInsufficientBalance is the deny code when a prepaid account lacks the
// available balance to cover the estimate.
const DenyInsufficientBalance = "insufficient_balance"

// AuthorizeSpend evaluates whether an estimated charge is permitted for an
// payer: the per-account/per-actor spend policy (CheckSpendAllowed) AND, for
// prepaid accounts, available balance. It does NOT move money. (#235)
func (s *Service) AuthorizeSpend(ctx context.Context, req AuthorizeSpendRequest) (*AuthorizeSpendResult, error) {
	if req.EstimateMicros < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	if req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}

	snap, err := s.GetCreditAccount(ctx, req.CustomerID, "")
	if err != nil {
		return nil, err
	}
	dec, err := s.moneyService().CheckSpendAllowed(ctx, req.CustomerID, strings.TrimSpace(req.Actor), req.EstimateMicros)
	if err != nil {
		return nil, err
	}

	res := &AuthorizeSpendResult{
		Allowed:               dec.Allowed,
		DenyCode:              dec.DenyCode,
		BillingMode:           snap.BillingMode,
		AvailableMicros:       snap.AvailableMicros,
		OutstandingOwedMicros: snap.OutstandingOwedMicros,
		RetryAfterSeconds:     dec.RetryAfterSeconds,
		NextAllowedAt:         dec.NextAllowedAt,
		Caps:                  dec.Caps,
	}

	// Prepaid accounts are additionally gated on available balance; arrears
	// accounts are gated by the outstanding ceiling inside CheckSpendAllowed.
	if snap.BillingMode != money.BillingModeArrears && req.EstimateMicros > snap.AvailableMicros {
		res.Allowed = false
		if res.DenyCode == "" {
			res.DenyCode = DenyInsufficientBalance
		}
	}
	return res, nil
}

// AuthorizeAndHoldRequest is the input to AuthorizeAndHold — the atomic
// policy-decision + hold placement that backs POST /v1/service/credits/authorize
// (issue #235/#247). Payer is the tenant subject billed; Actor is the canonical
// actor for per-actor sub-budgets; RequestID is the idempotency key for the
// placed hold.
type AuthorizeAndHoldRequest struct {
	CustomerID identity.CustomerID
	Actor      string
	// Currency is the ledger unit; empty defaults to "USD" (#476).
	Currency       string
	EstimateMicros int64
	RequestID      string
	// ExpiresAt bounds the placed hold; when zero a default TTL is applied.
	ExpiresAt time.Time
}

// AuthorizeAndHoldResult is the combined decision + reservation returned by
// AuthorizeAndHold. ReservationID is the placed hold's id when Allowed, else nil.
type AuthorizeAndHoldResult struct {
	Allowed               bool              `json:"allowed"`
	DenyCode              string            `json:"deny_code,omitempty"`
	BillingMode           string            `json:"billing_mode"`
	AvailableMicros       int64             `json:"available_micros"`
	OutstandingOwedMicros int64             `json:"outstanding_owed_micros"`
	RemainingTodayMicros  *int64            `json:"remaining_today_micros,omitempty"`
	RetryAfterSeconds     int64             `json:"retry_after_seconds,omitempty"`
	ReservationID         *uuid.UUID        `json:"reservation_id,omitempty"`
	Caps                  []money.CapResult `json:"caps,omitempty"`
}

// authorizeHoldSource is the source label recorded on holds placed by the
// authorize route, so they are attributable + idempotency-keyed by request_id.
const authorizeHoldSource = "authorize"

// defaultAuthorizeHoldTTL bounds a placed hold when the caller does not supply an
// explicit expiry. A hold is a short-lived reservation against an in-flight
// invocation; the reconcile/orphan-hold sweeper (#243) is the backstop.
const defaultAuthorizeHoldTTL = 15 * time.Minute

// AuthorizeAndHold runs the spend-policy decision + prepaid available-balance
// gate AND, when allowed, places the hold — ATOMICALLY, in one transaction
// (issue #235/#247). Unlike AuthorizeSpend (a read-only decision) followed by a
// separate HoldCredits, this cannot let two concurrent authorizes both pass on
// the same available balance: the balance row is locked for the duration of the
// decision + hold. Idempotent on RequestID.
func (s *Service) AuthorizeAndHold(ctx context.Context, req AuthorizeAndHoldRequest) (*AuthorizeAndHoldResult, error) {
	if req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if req.EstimateMicros < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	expires := req.ExpiresAt
	if expires.IsZero() {
		expires = s.now().Add(defaultAuthorizeHoldTTL).UTC()
	}

	out, err := s.moneyService().AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer:          req.CustomerID,
		Actor:          strings.TrimSpace(req.Actor),
		Currency:       req.Currency,
		EstimateMicros: req.EstimateMicros,
		Source:         authorizeHoldSource,
		SourceID:       req.RequestID,
		ExpiresAt:      expires,
	})
	if err != nil {
		return nil, err
	}

	res := &AuthorizeAndHoldResult{
		Allowed:               out.Decision.Allowed,
		DenyCode:              out.Decision.DenyCode,
		BillingMode:           out.BillingMode,
		AvailableMicros:       out.AvailableMicros,
		OutstandingOwedMicros: out.OutstandingOwedMicros,
		RetryAfterSeconds:     out.Decision.RetryAfterSeconds,
		Caps:                  out.Decision.Caps,
	}
	if r := remainingTodayCents(out.Decision.Caps); r != nil {
		res.RemainingTodayMicros = r
	}
	if out.Hold != nil {
		id := out.Hold.ID
		res.ReservationID = &id
	}
	return res, nil
}

// remainingTodayCents extracts the remaining headroom under a daily cap (org or
// per-actor) from the evaluated caps, for the authorize response's
// remaining_today_micros field. Returns nil when no daily cap applies.
func remainingTodayCents(caps []money.CapResult) *int64 {
	for _, c := range caps {
		if c.Code == money.DenyDailyCap || c.Code == money.DenyActorDailyCap {
			r := c.Remaining
			if r < 0 {
				r = 0
			}
			return &r
		}
	}
	return nil
}

// SetCreditAccountSettings upserts an payer's spend policy (issue #237/#235
// admin surface). Thin passthrough to the credits service.
func (s *Service) SetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, creditType string, in money.AccountSettingsInput) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	// Pin a tenant connection so the upsert sets the RLS GUC under openrails_app (#227).
	return s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.moneyService().UpsertAccountSettings(ctx, payer, in)
		return err
	})
}

// GetCreditAccountSettings returns an payer's stored credit-account settings
// (billing mode prepaid|arrears, spend caps, auto-top-up, expiry default) for the
// org billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, creditType string) (*models.MoneyAccount, error) {
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	return s.moneyService().GetAccountSettingsForCustomer(ctx, payer)
}

// GetCustomerCreditTransactions lists a tenant subject's credit transactions (usage) for
// the billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetCustomerCreditTransactions(ctx context.Context, payer identity.CustomerID, creditType string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	var items []models.MoneyTransaction
	var total int
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		items, total, e = s.moneyService().GetTransactionsByCustomer(ctx, payer, money.DefaultCurrency, limit, offset)
		return e
	})
	return items, total, err
}

// SetSpendLimit upserts a per-actor spend cap under an payer (issue #237).
func (s *Service) SetSpendLimit(ctx context.Context, payer identity.CustomerID, creditType, actor string, maxDay, maxMonth *int64) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	_, err := s.moneyService().SetSpendLimit(ctx, payer, actor, maxDay, maxMonth)
	return err
}
