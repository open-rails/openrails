package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
)

// CreditAccountSnapshot is the balance + policy view of an owner's credit
// account, served by the authorize/balance route (issue #235).
type CreditAccountSnapshot struct {
	OwnerID              uuid.UUID `json:"owner_id"`
	CreditType           string    `json:"credit_type"`
	BillingMode          string    `json:"billing_mode"`
	BalanceCents         int64     `json:"balance_cents"`
	HeldCents            int64     `json:"held_cents"`
	AvailableCents       int64     `json:"available_cents"`
	OutstandingOwedCents int64     `json:"outstanding_owed_cents"`
}

// GetCreditAccount returns the balance + policy snapshot for an owner org.
func (s *Service) GetCreditAccount(ctx context.Context, owner identity.OwnerOrgID, creditType string) (*CreditAccountSnapshot, error) {
	creditType = strings.TrimSpace(creditType)
	if creditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	// Pin a tenant-scoped connection so the balance/settings reads set the RLS GUC
	// and are tenant-scoped under the openrails_app role (#227). RunInTenantConn
	// reuses the request's already-pinned connection when the HTTP middleware set
	// one, so this is safe whether called from a request or directly.
	var snap *CreditAccountSnapshot
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		bal, err := s.creditsService().GetBalanceForOwner(ctx, owner, creditType)
		if err != nil {
			return err
		}
		settings, err := s.creditsService().GetAccountSettings(ctx, owner, creditType)
		if err != nil {
			return err
		}
		snap = &CreditAccountSnapshot{
			OwnerID:              owner.UUID(),
			CreditType:           creditType,
			BillingMode:          settings.BillingMode,
			BalanceCents:         bal.Balance,
			HeldCents:            bal.HeldBalance,
			AvailableCents:       bal.Balance - bal.HeldBalance,
			OutstandingOwedCents: settings.OutstandingOwedCents,
		}
		return nil
	})
	return snap, err
}

// UsageRow is one per-event_type usage rollup over a window: summed host-priced
// amount, event count, and summed per-dimension counts (issue #289). It mirrors
// credits.UsageRollupRow but lives on the public facade so HTTP/library callers
// don't import the internal credits package.
type UsageRow struct {
	EventType   string           `json:"event_type"`
	TotalAmount int64            `json:"total_amount"`
	EventCount  int64            `json:"event_count"`
	Dimensions  map[string]int64 `json:"dimensions"`
}

// GetUsage rolls up an owner's usage events over [from, to) grouped by
// event_type with summed dimensions (issue #289). Like GetCreditAccount it pins
// a tenant-scoped connection so the rollup query runs RLS-scoped under the
// openrails_app role (#227); RunInTenantConn reuses the request's already-pinned
// connection when one is set.
func (s *Service) GetUsage(ctx context.Context, owner identity.OwnerOrgID, from, to time.Time) ([]UsageRow, error) {
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	var out []UsageRow
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, err := s.creditsService().AggregateUsage(ctx, owner, from, to)
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

// ListInvoices lists an owner's finalized invoices, newest period first,
// paginated (issue #303). Like GetUsage it pins a tenant-scoped connection so the
// read runs RLS-scoped under the openrails_app role (#227); RunInTenantConn
// reuses the request's already-pinned connection when one is set. Returns the
// page of public DTOs plus the total count for pagination.
func (s *Service) ListInvoices(ctx context.Context, owner identity.OwnerOrgID, limit, offset int) ([]InvoiceDTO, int, error) {
	if owner.IsZero() {
		return nil, 0, fmt.Errorf("owner required")
	}
	var out []InvoiceDTO
	var total int
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, t, err := s.creditsService().ListInvoices(ctx, owner, limit, offset)
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

// GetInvoice returns one finalized invoice (with its line items) for an owner by
// id (issue #303). RLS-scoped like ListInvoices; an invoice belonging to another
// owner/tenant is unreachable (fail closed). Returns a public DTO.
func (s *Service) GetInvoice(ctx context.Context, owner identity.OwnerOrgID, id uuid.UUID) (*InvoiceDTO, error) {
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	var out *InvoiceDTO
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		inv, err := s.creditsService().GetInvoiceByID(ctx, owner, id)
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
	OwnerID       *identity.OwnerOrgID
	UserID        string // actor; used to resolve the owner when OwnerID is nil
	CreditType    string
	Invoker       string // canonical invoker for per-invoker caps ('oat:...', 'user:...', 'issuer:sub')
	EstimateCents int64
}

// AuthorizeSpendResult is the decision returned to the caller (mirrors the
// authorize route payload).
type AuthorizeSpendResult struct {
	Allowed              bool                `json:"allowed"`
	DenyCode             string              `json:"deny_code,omitempty"`
	BillingMode          string              `json:"billing_mode"`
	AvailableCents       int64               `json:"available_cents"`
	OutstandingOwedCents int64               `json:"outstanding_owed_cents"`
	RetryAfterSeconds    int64               `json:"retry_after_seconds,omitempty"`
	NextAllowedAt        *time.Time          `json:"next_allowed_at,omitempty"`
	Caps                 []credits.CapResult `json:"caps,omitempty"`
}

// DenyInsufficientBalance is the deny code when a prepaid account lacks the
// available balance to cover the estimate.
const DenyInsufficientBalance = "insufficient_balance"

// AuthorizeSpend evaluates whether an estimated charge is permitted for an
// owner: the per-account/per-invoker spend policy (CheckSpendAllowed) AND, for
// prepaid accounts, available balance. It does NOT move money. (#235)
func (s *Service) AuthorizeSpend(ctx context.Context, req AuthorizeSpendRequest) (*AuthorizeSpendResult, error) {
	req.CreditType = strings.TrimSpace(req.CreditType)
	if req.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if req.EstimateCents < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	owner := identity.OwnerOrgID{}
	if req.OwnerID != nil && !req.OwnerID.IsZero() {
		owner = *req.OwnerID
	} else {
		owner = identity.OwnerOrgIDFromString(strings.TrimSpace(req.UserID))
	}
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required (explicit OwnerID or UUID UserID)")
	}

	snap, err := s.GetCreditAccount(ctx, owner, req.CreditType)
	if err != nil {
		return nil, err
	}
	dec, err := s.creditsService().CheckSpendAllowed(ctx, owner, req.CreditType, strings.TrimSpace(req.Invoker), req.EstimateCents)
	if err != nil {
		return nil, err
	}

	res := &AuthorizeSpendResult{
		Allowed:              dec.Allowed,
		DenyCode:             dec.DenyCode,
		BillingMode:          snap.BillingMode,
		AvailableCents:       snap.AvailableCents,
		OutstandingOwedCents: snap.OutstandingOwedCents,
		RetryAfterSeconds:    dec.RetryAfterSeconds,
		NextAllowedAt:        dec.NextAllowedAt,
		Caps:                 dec.Caps,
	}

	// Prepaid accounts are additionally gated on available balance; arrears
	// accounts are gated by the outstanding ceiling inside CheckSpendAllowed.
	if snap.BillingMode != credits.BillingModeArrears && req.EstimateCents > snap.AvailableCents {
		res.Allowed = false
		if res.DenyCode == "" {
			res.DenyCode = DenyInsufficientBalance
		}
	}
	return res, nil
}

// AuthorizeAndHoldRequest is the input to AuthorizeAndHold — the atomic
// policy-decision + hold placement that backs POST /v1/service/credits/authorize
// (issue #235/#247). Payer is the owner org billed; Invoker is the canonical
// actor for per-invoker sub-budgets; RequestID is the idempotency key for the
// placed hold.
type AuthorizeAndHoldRequest struct {
	Payer         identity.OwnerOrgID
	Invoker       string
	CreditType    string
	EstimateCents int64
	RequestID     string
	// ExpiresAt bounds the placed hold; when zero a default TTL is applied.
	ExpiresAt time.Time
}

// AuthorizeAndHoldResult is the combined decision + reservation returned by
// AuthorizeAndHold. ReservationID is the placed hold's id when Allowed, else nil.
type AuthorizeAndHoldResult struct {
	Allowed              bool                `json:"allowed"`
	DenyCode             string              `json:"deny_code,omitempty"`
	BillingMode          string              `json:"billing_mode"`
	AvailableCents       int64               `json:"available_cents"`
	OutstandingOwedCents int64               `json:"outstanding_owed_cents"`
	RemainingTodayCents  *int64              `json:"remaining_today_cents,omitempty"`
	RetryAfterSeconds    int64               `json:"retry_after_seconds,omitempty"`
	ReservationID        *uuid.UUID          `json:"reservation_id,omitempty"`
	Caps                 []credits.CapResult `json:"caps,omitempty"`
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
	req.CreditType = strings.TrimSpace(req.CreditType)
	if req.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if req.Payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if req.EstimateCents < 0 {
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

	out, err := s.creditsService().AuthorizeAndHold(ctx, credits.AuthorizeHoldInput{
		Owner:         req.Payer,
		Invoker:       strings.TrimSpace(req.Invoker),
		CreditType:    req.CreditType,
		EstimateCents: req.EstimateCents,
		Source:        authorizeHoldSource,
		SourceID:      req.RequestID,
		ExpiresAt:     expires,
	})
	if err != nil {
		return nil, err
	}

	res := &AuthorizeAndHoldResult{
		Allowed:              out.Decision.Allowed,
		DenyCode:             out.Decision.DenyCode,
		BillingMode:          out.BillingMode,
		AvailableCents:       out.AvailableCents,
		OutstandingOwedCents: out.OutstandingOwedCents,
		RetryAfterSeconds:    out.Decision.RetryAfterSeconds,
		Caps:                 out.Decision.Caps,
	}
	if r := remainingTodayCents(out.Decision.Caps); r != nil {
		res.RemainingTodayCents = r
	}
	if out.Hold != nil {
		id := out.Hold.ID
		res.ReservationID = &id
	}
	return res, nil
}

// remainingTodayCents extracts the remaining headroom under a daily cap (org or
// per-invoker) from the evaluated caps, for the authorize response's
// remaining_today_cents field. Returns nil when no daily cap applies.
func remainingTodayCents(caps []credits.CapResult) *int64 {
	for _, c := range caps {
		if c.Code == credits.DenyDailyCap || c.Code == credits.DenyInvokerDailyCap {
			r := c.Remaining
			if r < 0 {
				r = 0
			}
			return &r
		}
	}
	return nil
}

// SetCreditAccountSettings upserts an owner's spend policy (issue #237/#235
// admin surface). Thin passthrough to the credits service.
func (s *Service) SetCreditAccountSettings(ctx context.Context, owner identity.OwnerOrgID, creditType string, in credits.AccountSettingsInput) error {
	if owner.IsZero() {
		return fmt.Errorf("owner required")
	}
	// Pin a tenant connection so the upsert sets the RLS GUC under openrails_app (#227).
	return s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.creditsService().UpsertAccountSettings(ctx, owner, strings.TrimSpace(creditType), in)
		return err
	})
}

// GetCreditAccountSettings returns an owner's stored credit-account settings
// (billing mode prepaid|arrears, spend caps, auto-top-up, expiry default) for the
// org billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetCreditAccountSettings(ctx context.Context, owner identity.OwnerOrgID, creditType string) (*models.CreditAccountSettings, error) {
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	return s.creditsService().GetAccountSettingsForOwner(ctx, owner, strings.TrimSpace(creditType))
}

// GetOwnerCreditTransactions lists an owner org's credit transactions (usage) for
// the billing-account admin surface (issue #242). RLS-scoped.
func (s *Service) GetOwnerCreditTransactions(ctx context.Context, owner identity.OwnerOrgID, creditType string, limit, offset int) ([]models.CreditTransaction, int, error) {
	if owner.IsZero() {
		return nil, 0, fmt.Errorf("owner required")
	}
	var items []models.CreditTransaction
	var total int
	err := s.rt.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		items, total, e = s.creditsService().GetTransactionsByOwner(ctx, owner, strings.TrimSpace(creditType), limit, offset)
		return e
	})
	return items, total, err
}

// SetSpendLimit upserts a per-invoker spend cap under an owner (issue #237).
func (s *Service) SetSpendLimit(ctx context.Context, owner identity.OwnerOrgID, creditType, invoker string, maxDay, maxMonth *int64) error {
	if owner.IsZero() {
		return fmt.Errorf("owner required")
	}
	_, err := s.creditsService().SetSpendLimit(ctx, owner, strings.TrimSpace(creditType), invoker, maxDay, maxMonth)
	return err
}
