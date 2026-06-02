package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

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
	bal, err := s.creditsService().GetBalanceForOwner(ctx, owner, creditType)
	if err != nil {
		return nil, err
	}
	settings, err := s.creditsService().GetAccountSettings(ctx, owner, creditType)
	if err != nil {
		return nil, err
	}
	return &CreditAccountSnapshot{
		OwnerID:              owner.UUID(),
		CreditType:           creditType,
		BillingMode:          settings.BillingMode,
		BalanceCents:         bal.Balance,
		HeldCents:            bal.HeldBalance,
		AvailableCents:       bal.Balance - bal.HeldBalance,
		OutstandingOwedCents: settings.OutstandingOwedCents,
	}, nil
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

// SetCreditAccountSettings upserts an owner's spend policy (issue #237/#235
// admin surface). Thin passthrough to the credits service.
func (s *Service) SetCreditAccountSettings(ctx context.Context, owner identity.OwnerOrgID, creditType string, in credits.AccountSettingsInput) error {
	if owner.IsZero() {
		return fmt.Errorf("owner required")
	}
	_, err := s.creditsService().UpsertAccountSettings(ctx, owner, strings.TrimSpace(creditType), in)
	return err
}

// SetSpendLimit upserts a per-invoker spend cap under an owner (issue #237).
func (s *Service) SetSpendLimit(ctx context.Context, owner identity.OwnerOrgID, creditType, invoker string, maxDay, maxMonth *int64) error {
	if owner.IsZero() {
		return fmt.Errorf("owner required")
	}
	_, err := s.creditsService().SetSpendLimit(ctx, owner, strings.TrimSpace(creditType), invoker, maxDay, maxMonth)
	return err
}
