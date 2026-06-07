package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
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

func (s *Service) now() time.Time {
	if s != nil && s.rt != nil && s.rt.Clock != nil {
		return s.rt.Clock.Now()
	}
	return time.Now()
}

var ErrInsufficientCredits = credits.ErrInsufficientCredits
var ErrCreditTypeInactive = credits.ErrCreditTypeInactive

type HoldCreditsRequest struct {
	TenantSubjectID *identity.TenantSubjectID
	InvokerID       string
	CreditType      string
	Amount          int64
	Source          string
	SourceID        string
	ExpiresAt       time.Time
}

type CreditHold struct {
	ID        uuid.UUID
	InvokerID string
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
	req.InvokerID = strings.TrimSpace(req.InvokerID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	req.SourceID = strings.TrimSpace(req.SourceID)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.InvokerID == "" {
		return nil, fmt.Errorf("invoker_id required")
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

	hold, err := s.creditsService().Hold(ctx, req.TenantSubjectID, req.InvokerID, req.CreditType, req.Amount, req.Source, req.SourceID, req.ExpiresAt.UTC())
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
		InvokerID: hold.InvokerID,
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
	HoldID uuid.UUID
	Amount int64

	// Usage analytics (#311): when EventType is set, the capture ALSO appends a
	// billing.usage_events row linked to the capture transaction (no second
	// debit), so the platform's /budget-usage + revenue analytics can be served
	// from OpenRails. EventType is the metered model/endpoint; Metadata carries
	// the string grouping dims (function_name, availability_tier,
	// delegated_user_id, ...). Source/SourceID default to the capture's.
	EventType  string
	Dimensions map[string]int64
	Metadata   map[string]any
	Source     string
	SourceID   string
}

type CreditTransaction struct {
	ID              uuid.UUID
	TenantSubjectID uuid.UUID
	InvokerID       string
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
	TenantSubjectID *identity.TenantSubjectID
	InvokerID       string
	CreditType      string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
}

func (s *Service) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	req.InvokerID = strings.TrimSpace(req.InvokerID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.InvokerID == "" {
		return nil, fmt.Errorf("invoker_id required")
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
	if req.SourceID == nil || *req.SourceID == uuid.Nil {
		return nil, fmt.Errorf("source_id required")
	}
	trx, err := s.creditsService().Withdraw(ctx, credits.CreditWithdrawParams{
		TenantSubjectID: req.TenantSubjectID,
		InvokerID:       req.InvokerID,
		CreditType:      req.CreditType,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		TenantSubjectID: trx.TenantSubjectID,
		InvokerID:       trx.InvokerID,
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
	TenantSubjectID *identity.TenantSubjectID
	InvokerID       string
	CreditType      string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
	ExpiresAt       *time.Time
	Description     *string
}

func (s *Service) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	req.InvokerID = strings.TrimSpace(req.InvokerID)
	req.CreditType = strings.TrimSpace(req.CreditType)
	req.Source = strings.TrimSpace(req.Source)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.InvokerID == "" {
		return nil, fmt.Errorf("invoker_id required")
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
	if req.SourceID == nil || *req.SourceID == uuid.Nil {
		return nil, fmt.Errorf("source_id required")
	}
	trx, err := s.creditsService().Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: req.TenantSubjectID,
		InvokerID:       req.InvokerID,
		CreditType:      req.CreditType,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
		ExpiresAt:       req.ExpiresAt,
		Description:     req.Description,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		TenantSubjectID: trx.TenantSubjectID,
		InvokerID:       trx.InvokerID,
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
	if req.HoldID == uuid.Nil {
		return nil, fmt.Errorf("hold_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	trx, err := s.creditsService().CaptureHold(ctx, req.HoldID, req.Amount)
	if err != nil {
		return nil, err
	}
	// #311: append an analytics usage_event linked to this capture (no second
	// debit) so OpenRails is the source of truth for platform usage/revenue.
	if strings.TrimSpace(req.EventType) != "" {
		source := strings.TrimSpace(req.Source)
		if source == "" {
			source = strings.TrimSpace(trx.Source)
		}
		sourceID := strings.TrimSpace(req.SourceID)
		if sourceID == "" && trx.SourceID != nil {
			sourceID = strings.TrimSpace(*trx.SourceID)
		}
		captureTxnID := trx.ID
		if uerr := s.creditsService().InsertCaptureUsageEvent(ctx, credits.CaptureUsageEventParams{
			TenantSubjectID:     trx.TenantSubjectID,
			InvokerID:           trx.InvokerID,
			CreditTypeID:        trx.CreditTypeID,
			EventType:           req.EventType,
			Amount:              req.Amount,
			Dimensions:          req.Dimensions,
			Metadata:            req.Metadata,
			Source:              source,
			SourceID:            sourceID,
			CreditTransactionID: &captureTxnID,
		}); uerr != nil {
			// Analytics is best-effort: a usage_event failure must NOT fail the
			// capture (the money is already captured).
			log.Warnf("service capture: usage_event insert failed (capture %s kept): %v", trx.ID, uerr)
		}
	}
	return &CreditTransaction{
		ID:              trx.ID,
		TenantSubjectID: trx.TenantSubjectID,
		InvokerID:       trx.InvokerID,
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

// ServiceUsageRollupRow is one grouped spend bucket (dimension value, event
// count, summed host-priced amount).
type ServiceUsageRollupRow struct {
	Key         string `json:"key"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount"`
}

// ServiceUsageRollupRequest selects a payer + window + grouping dimension.
type ServiceUsageRollupRequest struct {
	TenantSubjectID *identity.TenantSubjectID
	From            time.Time
	To              time.Time
	GroupBy         string // endpoint | function | tier | user
}

// ServiceUsageRollup returns per-dimension-value spend for a payer over a
// window (#311) — the OpenRails-sourced data behind the platform's
// /budget-usage + revenue analytics. Service-scoped (operator service token).
func (s *Service) ServiceUsageRollup(ctx context.Context, req ServiceUsageRollupRequest) ([]ServiceUsageRollupRow, error) {
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	rows, err := s.creditsService().ServiceUsageRollup(ctx, *req.TenantSubjectID, req.From, req.To, req.GroupBy)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceUsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServiceUsageRollupRow{Key: r.Key, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
	}
	return out, nil
}

// EndpointRevenueDailyRow is one day's revenue (millicents) for an endpoint.
type EndpointRevenueDailyRow struct {
	Date             string `json:"date"`
	AmountMillicents int64  `json:"amount_millicents"`
}

// EndpointRevenueDaily returns per-day revenue for an endpoint (by metadata
// endpoint_name) across all payers in the tenant over [from, to) — powers
// tensorhub endpoint revenue analytics (#410).
func (s *Service) EndpointRevenueDaily(ctx context.Context, endpointName string, from, to time.Time) ([]EndpointRevenueDailyRow, error) {
	rows, err := s.creditsService().EndpointRevenueDaily(ctx, endpointName, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]EndpointRevenueDailyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, EndpointRevenueDailyRow{Date: r.Date, AmountMillicents: r.AmountMillicents})
	}
	return out, nil
}

func (s *Service) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if holdID == uuid.Nil {
		return fmt.Errorf("hold_id required")
	}
	return s.creditsService().ReleaseHold(ctx, holdID)
}

func (s *Service) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlements(ctx, userID, at.UTC())
}

func (s *Service) ListActiveEntitlementsForTenantSubject(ctx context.Context, tenantSubjectID identity.TenantSubjectID, at time.Time) ([]string, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlementsByTenantSubject(ctx, tenantSubjectID.UUID(), at.UTC())
}

func (s *Service) IsTenantSubjectEntitled(ctx context.Context, tenantSubjectID identity.TenantSubjectID, entitlement string, at time.Time) (bool, error) {
	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("tenant_subject_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().IsTenantSubjectEntitled(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

func (s *Service) HasActiveIndefiniteEntitlementForTenantSubject(ctx context.Context, tenantSubjectID identity.TenantSubjectID, entitlement string, at time.Time) (bool, error) {
	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("tenant_subject_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().HasActiveIndefiniteByTenantSubject(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

func (s *Service) LatestFiniteEntitlementWindowForTenantSubject(ctx context.Context, tenantSubjectID identity.TenantSubjectID, entitlement string, at time.Time) (*EntitlementRecord, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return nil, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	ent, err := s.entitlementService().LatestFiniteWindowByTenantSubject(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
	if err != nil {
		return nil, err
	}
	record := entitlementRecordFromEntitlement(ent)
	return &record, nil
}

type EntitlementRecord struct {
	ID              uuid.UUID
	TenantSubjectID uuid.UUID
	UserID          string
	Entitlement     string
	StartAt         time.Time
	EndAt           *time.Time
	SourceID        *uuid.UUID
	SourceType      string
	RevokedAt       *time.Time
	RevokeReason    *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (s *Service) ListActiveEntitlementRecords(ctx context.Context, userID string, at time.Time) ([]EntitlementRecord, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	records, err := s.entitlementService().ListActiveRecords(ctx, userID, at.UTC())
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
			ID:              e.ID,
			TenantSubjectID: e.TenantSubjectID,
			UserID:          e.UserID,
			Entitlement:     e.Entitlement,
			StartAt:         e.StartAt,
			EndAt:           e.EndAt,
			SourceID:        e.SourceID,
			SourceType:      string(e.SourceType),
			RevokedAt:       e.RevokedAt,
			RevokeReason:    reason,
			CreatedAt:       e.CreatedAt,
			UpdatedAt:       e.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Service) ListActiveEntitlementRecordsForTenantSubject(ctx context.Context, tenantSubjectID identity.TenantSubjectID, at time.Time) ([]EntitlementRecord, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	records, err := s.entitlementService().ListActiveRecordsByTenantSubject(ctx, tenantSubjectID.UUID(), at.UTC())
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
			ID:              e.ID,
			TenantSubjectID: e.TenantSubjectID,
			UserID:          e.UserID,
			Entitlement:     e.Entitlement,
			StartAt:         e.StartAt,
			EndAt:           e.EndAt,
			SourceID:        e.SourceID,
			SourceType:      string(e.SourceType),
			RevokedAt:       e.RevokedAt,
			RevokeReason:    reason,
			CreatedAt:       e.CreatedAt,
			UpdatedAt:       e.UpdatedAt,
		})
	}
	return out, nil
}
