package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/services"
)

// Service is the exported, in-process billing API.
//
// It is intended for embedded hosts that want to call billing logic directly, without going
// through the HTTP handlers. The standalone HTTP server should treat its routes as thin
// adapters over this API.
type Service struct {
	rt *app.Runtime
}

func New(rt *app.Runtime) (*Service, error) {
	if rt == nil {
		return nil, fmt.Errorf("billing service: runtime is nil")
	}
	if rt.CreditsService == nil {
		return nil, fmt.Errorf("billing service: credits service unavailable")
	}
	if rt.EntitlementService == nil {
		return nil, fmt.Errorf("billing service: entitlement service unavailable")
	}
	return &Service{rt: rt}, nil
}

var ErrInsufficientCredits = services.ErrInsufficientCredits
var ErrCreditTypeInactive = services.ErrCreditTypeInactive
var ErrInvalidNegativePolicy = services.ErrInvalidNegativePolicy
var ErrNegativeBalanceLimitExceeded = services.ErrNegativeBalanceLimitExceeded

type HoldCreditsRequest struct {
	UserID     string
	CreditType string
	Amount     int64
	Source     string
	SourceID   string
	ExpiresAt  time.Time
}

type CreditHold struct {
	ID        uuid.UUID
	UserID    string
	Amount    int64
	Source    string
	SourceID  string
	Status    string
	ExpiresAt time.Time
	Captured  *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Service) HoldCredits(ctx context.Context, req HoldCreditsRequest) (*CreditHold, error) {
	if s == nil || s.rt == nil || s.rt.CreditsService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	req.SourceID = strings.TrimSpace(req.SourceID)
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if req.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if req.Source == "" {
		return nil, fmt.Errorf("source required")
	}
	if req.SourceID == "" {
		return nil, fmt.Errorf("source_id required")
	}
	if req.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("expires_at required")
	}

	hold, err := s.rt.CreditsService.Hold(ctx, req.UserID, req.CreditType, req.Amount, req.Source, req.SourceID, req.ExpiresAt.UTC())
	if err != nil {
		return nil, err
	}
	amount := int64(0)
	if hold.Authorized != nil {
		amount = *hold.Authorized
	}
	expiresAt := time.Time{}
	if hold.ExpiresAt != nil {
		expiresAt = hold.ExpiresAt.UTC()
	}
	srcID := ""
	if hold.SourceID != nil {
		srcID = *hold.SourceID
	}
	return &CreditHold{
		ID:        hold.ID,
		UserID:    hold.UserID,
		Amount:    amount,
		Source:    hold.Source,
		SourceID:  srcID,
		Status:    hold.Status,
		ExpiresAt: expiresAt,
		Captured:  hold.Captured,
		CreatedAt: hold.CreatedAt,
		UpdatedAt: hold.UpdatedAt,
	}, nil
}

type CaptureHoldRequest struct {
	HoldID        uuid.UUID
	Amount        int64
	AllowNegative bool
	MaxNegative   int64
}

type CreditTransaction struct {
	ID              uuid.UUID
	UserID          string
	CreditTypeID    uuid.UUID
	Amount          int64
	BalanceAfter    *int64
	TransactionType string
	Status          string
	Authorized      *int64
	Captured        *int64
	Source          string
	SourceID        *string
	ExpiresAt       *time.Time
	Description     *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type WithdrawCreditsRequest struct {
	UserID        string
	CreditType    string
	Amount        int64
	Source        string
	SourceID      *uuid.UUID
	AllowNegative bool
	MaxNegative   int64
}

func (s *Service) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	if s == nil || s.rt == nil || s.rt.CreditsService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if req.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if req.Source == "" {
		return nil, fmt.Errorf("source required")
	}
	trx, err := s.rt.CreditsService.Withdraw(ctx, services.CreditWithdrawParams{
		UserID:        req.UserID,
		CreditType:    req.CreditType,
		Amount:        req.Amount,
		Source:        req.Source,
		SourceID:      req.SourceID,
		AllowNegative: req.AllowNegative,
		MaxNegative:   req.MaxNegative,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		UserID:          trx.UserID,
		CreditTypeID:    trx.CreditTypeID,
		Amount:          trx.Amount,
		BalanceAfter:    trx.BalanceAfter,
		TransactionType: trx.TransactionType,
		Status:          trx.Status,
		Authorized:      trx.Authorized,
		Captured:        trx.Captured,
		Source:          trx.Source,
		SourceID:        trx.SourceID,
		ExpiresAt:       trx.ExpiresAt,
		Description:     trx.Description,
		CreatedAt:       trx.CreatedAt,
		UpdatedAt:       trx.UpdatedAt,
	}, nil
}

type DepositCreditsRequest struct {
	UserID      string
	CreditType  string
	Amount      int64
	Source      string
	SourceID    *uuid.UUID
	ExpiresAt   *time.Time
	Description *string
}

func (s *Service) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	if s == nil || s.rt == nil || s.rt.CreditsService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if req.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if req.Source == "" {
		return nil, fmt.Errorf("source required")
	}
	trx, err := s.rt.CreditsService.Deposit(ctx, services.CreditDepositParams{
		UserID:      req.UserID,
		CreditType:  req.CreditType,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		ExpiresAt:   req.ExpiresAt,
		Description: req.Description,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		UserID:          trx.UserID,
		CreditTypeID:    trx.CreditTypeID,
		Amount:          trx.Amount,
		BalanceAfter:    trx.BalanceAfter,
		TransactionType: trx.TransactionType,
		Status:          trx.Status,
		Authorized:      trx.Authorized,
		Captured:        trx.Captured,
		Source:          trx.Source,
		SourceID:        trx.SourceID,
		ExpiresAt:       trx.ExpiresAt,
		Description:     trx.Description,
		CreatedAt:       trx.CreatedAt,
		UpdatedAt:       trx.UpdatedAt,
	}, nil
}

func (s *Service) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error) {
	if s == nil || s.rt == nil || s.rt.CreditsService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	if req.HoldID == uuid.Nil {
		return nil, fmt.Errorf("hold_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	trx, err := s.rt.CreditsService.CaptureHoldWithPolicy(ctx, req.HoldID, req.Amount, req.AllowNegative, req.MaxNegative)
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		UserID:          trx.UserID,
		CreditTypeID:    trx.CreditTypeID,
		Amount:          trx.Amount,
		BalanceAfter:    trx.BalanceAfter,
		TransactionType: trx.TransactionType,
		Status:          trx.Status,
		Authorized:      trx.Authorized,
		Captured:        trx.Captured,
		Source:          trx.Source,
		SourceID:        trx.SourceID,
		ExpiresAt:       trx.ExpiresAt,
		Description:     trx.Description,
		CreatedAt:       trx.CreatedAt,
		UpdatedAt:       trx.UpdatedAt,
	}, nil
}

func (s *Service) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if s == nil || s.rt == nil || s.rt.CreditsService == nil {
		return fmt.Errorf("billing service: not initialized")
	}
	if holdID == uuid.Nil {
		return fmt.Errorf("hold_id required")
	}
	return s.rt.CreditsService.ReleaseHold(ctx, holdID)
}

func (s *Service) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	if s == nil || s.rt == nil || s.rt.EntitlementService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.rt.EntitlementService.ListActiveEntitlements(ctx, userID, at.UTC())
}

type EntitlementRecord struct {
	ID           uuid.UUID
	UserID       string
	Entitlement  string
	StartAt      time.Time
	EndAt        *time.Time
	SourceID     *uuid.UUID
	SourceType   string
	RevokedAt    *time.Time
	RevokeReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (s *Service) ListActiveEntitlementRecords(ctx context.Context, userID string, at time.Time) ([]EntitlementRecord, error) {
	if s == nil || s.rt == nil || s.rt.EntitlementService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	records, err := s.rt.EntitlementService.ListActiveRecords(ctx, userID, at.UTC())
	if err != nil {
		return nil, err
	}
	out := make([]EntitlementRecord, 0, len(records))
	for _, e := range records {
		reason := (*string)(nil)
		if e.RevokeReason != nil {
			v := string(*e.RevokeReason)
			reason = &v
		}
		out = append(out, EntitlementRecord{
			ID:           e.ID,
			UserID:       e.UserID,
			Entitlement:  e.Entitlement,
			StartAt:      e.StartAt,
			EndAt:        e.EndAt,
			SourceID:     e.SourceID,
			SourceType:   string(e.SourceType),
			RevokedAt:    e.RevokedAt,
			RevokeReason: reason,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
		})
	}
	return out, nil
}
