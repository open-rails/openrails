package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
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
	if rt.MoneyService == nil {
		return nil, fmt.Errorf("billing service: money service unavailable")
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

var ErrInsufficientCredits = money.ErrInsufficientCredits

// ErrCreditTypeInactive is retained as a facade sentinel (#472): money has a
// single implicit unit, so it is never returned anymore, but the public error
// surface (and its handler/embed consumers) is kept stable.
var ErrCreditTypeInactive = errors.New("credit_type_inactive")

type HoldCreditsRequest struct {
	TenantSubjectID *identity.TenantSubjectID
	Actor           string
	Amount          int64
	Source          string
	SourceID        string
	ExpiresAt       time.Time
}

type CreditHold struct {
	ID        uuid.UUID
	Actor     string
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
	req.Actor = strings.TrimSpace(req.Actor)
	req.Source = strings.TrimSpace(req.Source)
	req.SourceID = strings.TrimSpace(req.SourceID)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.Actor == "" {
		return nil, fmt.Errorf("actor required")
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

	hold, err := s.moneyService().Hold(ctx, req.TenantSubjectID, req.Actor, money.DefaultCurrency, req.Amount, req.Source, req.SourceID, req.ExpiresAt.UTC())
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
		Actor:     hold.Actor,
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
	// openrails.usage_events row linked to the capture transaction (no second
	// debit), so the platform's /budget-usage + revenue analytics can be served
	// from OpenRails. EventType is the metered event kind; Resource is the
	// caller-supplied what-was-it-for string; Metadata carries long-tail string
	// dims (function_name, availability_tier, ...). Source/SourceID default to
	// the capture's.
	EventType string
	// Resource is the caller-supplied free-form string for what was metered
	// (opaque; e.g. tensorhub maps its endpoint slug here). Optional.
	Resource   string
	Dimensions map[string]int64
	Metadata   map[string]any
	Source     string
	SourceID   string
}

type CreditTransaction struct {
	ID              uuid.UUID
	TenantSubjectID uuid.UUID
	Actor           string
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
	Actor           string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
}

func (s *Service) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	req.Actor = strings.TrimSpace(req.Actor)
	req.Source = strings.TrimSpace(req.Source)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.Actor == "" {
		return nil, fmt.Errorf("actor required")
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
	trx, err := s.moneyService().Withdraw(ctx, money.WithdrawParams{
		TenantSubjectID: req.TenantSubjectID,
		Actor:           req.Actor,
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
		Actor:           trx.Actor,
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
	Actor           string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
	ExpiresAt       *time.Time
	Description     *string
}

func (s *Service) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	req.Actor = strings.TrimSpace(req.Actor)
	req.Source = strings.TrimSpace(req.Source)
	if req.TenantSubjectID == nil || req.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.Actor == "" {
		return nil, fmt.Errorf("actor required")
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
	trx, err := s.moneyService().Deposit(ctx, money.DepositParams{
		TenantSubjectID: req.TenantSubjectID,
		Actor:           req.Actor,
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
		Actor:           trx.Actor,
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
	trx, err := s.moneyService().CaptureHold(ctx, req.HoldID, req.Amount)
	if err != nil {
		return nil, err
	}
	// #473: settle ALL composed budget-scope reservations for this request to
	// "captured" by the hold's (payer, source, source_id) coords.
	s.captureBudgetScopes(ctx, trx, req.Amount)
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
		if uerr := s.moneyService().InsertCaptureUsageEvent(ctx, money.CaptureUsageEventParams{
			TenantSubjectID:    trx.TenantSubjectID,
			Actor:              trx.Actor,
			EventType:          req.EventType,
			Resource:           strings.TrimSpace(req.Resource),
			Amount:             req.Amount,
			Dimensions:         req.Dimensions,
			Metadata:           req.Metadata,
			Source:             source,
			SourceID:           sourceID,
			MoneyTransactionID: &captureTxnID,
		}); uerr != nil {
			// Analytics is best-effort: a usage_event failure must NOT fail the
			// capture (the money is already captured).
			log.Warnf("service capture: usage_event insert failed (capture %s kept): %v", trx.ID, uerr)
		}
	}
	return &CreditTransaction{
		ID:              trx.ID,
		TenantSubjectID: trx.TenantSubjectID,
		Actor:           trx.Actor,
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
	rows, err := s.moneyService().ServiceUsageRollup(ctx, *req.TenantSubjectID, req.From, req.To, req.GroupBy)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceUsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServiceUsageRollupRow{Key: r.Key, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
	}
	return out, nil
}

// ResourceRevenueDailyRow is one day's revenue (micros) for an endpoint.
type ResourceRevenueDailyRow struct {
	Date         string `json:"date"`
	AmountMicros int64  `json:"amount_micros"`
}

// ResourceRevenueDaily returns per-day revenue for a resource (typed
// attribution column) across all payers in the tenant over [from, to) — powers
// tensorhub endpoint revenue analytics (#410).
func (s *Service) ResourceRevenueDaily(ctx context.Context, resource string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	rows, err := s.moneyService().ResourceRevenueDaily(ctx, resource, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceRevenueDailyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ResourceRevenueDailyRow{Date: r.Date, AmountMicros: r.AmountMicros})
	}
	return out, nil
}

func (s *Service) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if holdID == uuid.Nil {
		return fmt.Errorf("hold_id required")
	}
	trx, err := s.moneyService().ReleaseHold(ctx, holdID)
	if err != nil {
		return err
	}
	// #473: free ALL composed budget-scope reservations for this request (every
	// scope reserved under the hold's (payer, source, source_id)). Idempotent.
	s.releaseBudgetScopes(ctx, trx)
	return nil
}

// releaseBudgetScopes settles every budget-scope reservation for a released
// hold to "released" by its (payer, source, source_id) coords (#473). Budget
// settlement is best-effort: the money hold is already released, so a budget
// settle failure must not fail the release (a stale active budget row self-heals
// at its reservation TTL).
func (s *Service) releaseBudgetScopes(ctx context.Context, trx *models.MoneyTransaction) {
	if trx == nil || trx.SourceID == nil || strings.TrimSpace(*trx.SourceID) == "" {
		return
	}
	payer := identity.TenantSubjectID(trx.TenantSubjectID)
	bsvc := budgets.NewService(s.rt.DB)
	if err := bsvc.ReleaseByCoords(ctx, payer, trx.Source, *trx.SourceID); err != nil {
		log.Warnf("service release: budget scope release failed (hold %s released): %v", trx.ID, err)
	}
	s.releaseQueueUnits(ctx, trx)
}

// releaseQueueUnits frees any batch/queue reservation units this request held
// (#472 G2c) by its (source, source_id) coords — a completed OR failed job frees
// its queued units. Best-effort + idempotent (no-op when no queue hold exists).
func (s *Service) releaseQueueUnits(ctx context.Context, trx *models.MoneyTransaction) {
	if trx == nil || trx.SourceID == nil || strings.TrimSpace(*trx.SourceID) == "" {
		return
	}
	if s.rt.RedisClient == nil {
		return
	}
	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	if err := lim.ReleaseQueueByRequest(ctx, trx.Source, *trx.SourceID); err != nil {
		log.Warnf("service settle: queue unit release failed (hold %s): %v", trx.ID, err)
	}
}

// captureBudgetScopes settles every budget-scope reservation for a captured
// hold to "captured" with the captured amount by its (payer, source, source_id)
// coords (#473). Best-effort, mirroring releaseBudgetScopes.
func (s *Service) captureBudgetScopes(ctx context.Context, trx *models.MoneyTransaction, capturedMicros int64) {
	if trx == nil || trx.SourceID == nil || strings.TrimSpace(*trx.SourceID) == "" {
		return
	}
	payer := identity.TenantSubjectID(trx.TenantSubjectID)
	bsvc := budgets.NewService(s.rt.DB)
	if err := bsvc.CaptureByCoords(ctx, payer, trx.Source, *trx.SourceID, capturedMicros); err != nil {
		log.Warnf("service capture: budget scope capture failed (hold %s captured): %v", trx.ID, err)
	}
	// A completed (captured) job also frees its queued units (#472 G2c).
	s.releaseQueueUnits(ctx, trx)
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
			UserID:          e.TenantSubjectID.String(),
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
			UserID:          e.TenantSubjectID.String(),
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

// EntitlementsBatchMaxSubjects bounds one batch read (#354); over-cap is an
// explicit error at the edge, never a silent truncation. Shared by the
// standalone handler and the embedded transport.
const EntitlementsBatchMaxSubjects = 500

// ListActiveEntitlementRecordsByExternalSubjects (#354): active rows for many
// external subjects of one (ambient-tenant, issuer) in one query, grouped by
// subject; subjects with no rows are absent. Callers trim/dedupe/cap.
func (s *Service) ListActiveEntitlementRecordsByExternalSubjects(ctx context.Context, issuer string, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return nil, fmt.Errorf("issuer required")
	}
	if len(subjects) == 0 {
		return map[string][]EntitlementRecord{}, nil
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	grouped, err := s.entitlementService().ListActiveRecordsByExternalSubjects(ctx, issuer, subjects, at.UTC())
	if err != nil {
		return nil, err
	}
	out := make(map[string][]EntitlementRecord, len(grouped))
	for subject, records := range grouped {
		rs := make([]EntitlementRecord, 0, len(records))
		for _, e := range records {
			reason := (*string)(nil)
			if e.RevokeReason != nil {
				v := string(*e.RevokeReason)
				reason = &v
			}
			rs = append(rs, EntitlementRecord{
				ID:              e.ID,
				TenantSubjectID: e.TenantSubjectID,
				UserID:          e.TenantSubjectID.String(),
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
		out[subject] = rs
	}
	return out, nil
}
