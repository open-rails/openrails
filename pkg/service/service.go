package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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

type HoldCreditsRequest struct {
	CustomerID  *identity.CustomerID
	Invoker     string
	InvokerType string
	Currency    string
	Amount      int64
	Source      string
	RequestID   string
	Resource    string
	ExpiresAt   time.Time
}

type CreditHold struct {
	RequestID string
	Invoker   string
	Currency  string
	Amount    int64
	Source    string
	Resource  string
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (s *Service) HoldCredits(ctx context.Context, req HoldCreditsRequest) (*CreditHold, error) {
	req.Invoker = strings.TrimSpace(req.Invoker)
	req.InvokerType = strings.TrimSpace(req.InvokerType)
	req.Source = strings.TrimSpace(req.Source)
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.CustomerID == nil || req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if req.Invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if req.Source == "" {
		return nil, fmt.Errorf("source required")
	}
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	if req.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("expires_at required")
	}
	if req.InvokerType != string(identity.InvokerTypePayer) && req.InvokerType != string(identity.InvokerTypeDelegated) {
		return nil, fmt.Errorf("invoker_type must be payer or delegated")
	}

	out, err := s.moneyService().AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer:           *req.CustomerID,
		Invoker:         req.Invoker,
		Currency:        currency,
		EstimatedAmount: req.Amount,
		Source:          req.Source,
		SourceID:        req.RequestID,
		ExpiresAt:       req.ExpiresAt.UTC(),
	})
	if err != nil {
		return nil, err
	}
	if !out.Decision.Allowed {
		return nil, ErrInsufficientCredits
	}

	store, err := s.holdStore()
	if err != nil {
		return nil, err
	}
	mid, err := serviceMerchantID(ctx)
	if err != nil {
		return nil, err
	}
	allowed, _, err := store.Place(ctx, holds.Hold{
		MerchantID:      mid,
		RequestID:       req.RequestID,
		CustomerID:      req.CustomerID.UUID().String(),
		Invoker:         req.Invoker,
		InvokerType:     req.InvokerType,
		Currency:        currency,
		EstimatedAmount: req.Amount,
		Source:          req.Source,
		Resource:        strings.TrimSpace(req.Resource),
		CreatedAt:       s.now().UTC(),
		ExpiresAt:       req.ExpiresAt.UTC(),
	}, out.AccountCapacityAmount)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrInsufficientCredits
	}
	return &CreditHold{
		RequestID: req.RequestID,
		Invoker:   req.Invoker,
		Currency:  currency,
		Amount:    req.Amount,
		Source:    req.Source,
		Resource:  strings.TrimSpace(req.Resource),
		ExpiresAt: req.ExpiresAt.UTC(),
		CreatedAt: s.now().UTC(),
	}, nil
}

type CaptureHoldRequest struct {
	RequestID string
	Amount    int64

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
	CustomerID      uuid.UUID
	Invoker         string
	Currency        string
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
	CustomerID *identity.CustomerID
	Invoker    string
	Currency   string
	Amount     int64
	Source     string
	SourceID   *uuid.UUID
}

func (s *Service) WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error) {
	req.Invoker = strings.TrimSpace(req.Invoker)
	req.Source = strings.TrimSpace(req.Source)
	if req.CustomerID == nil || req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if req.Invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
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
		CustomerID: req.CustomerID,
		Invoker:    req.Invoker,
		Currency:   currency,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        trx.Currency,
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
	CustomerID  *identity.CustomerID
	Invoker     string
	Currency    string
	Amount      int64
	Source      string
	SourceID    *uuid.UUID
	ExpiresAt   *time.Time
	Description *string
}

func (s *Service) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	req.Invoker = strings.TrimSpace(req.Invoker)
	req.Source = strings.TrimSpace(req.Source)
	if req.CustomerID == nil || req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if req.Invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
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
	// #491: DepositParams.SourceID is the natural-key string (uuidv7 pk + UNIQUE
	// natural key); the SDK request carries a uuid, so stringify it.
	depositSourceID := req.SourceID.String()
	trx, err := s.moneyService().Deposit(ctx, money.DepositParams{
		CustomerID:  req.CustomerID,
		Invoker:     req.Invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    &depositSourceID,
		ExpiresAt:   req.ExpiresAt,
		Description: req.Description,
	})
	if err != nil {
		return nil, err
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        trx.Currency,
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
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	store, err := s.holdStore()
	if err != nil {
		return nil, err
	}
	mid, err := serviceMerchantID(ctx)
	if err != nil {
		return nil, err
	}
	hold, err := store.Get(ctx, mid, req.RequestID)
	if err != nil {
		return nil, err
	}
	if req.Amount > hold.EstimatedAmount {
		return nil, fmt.Errorf("capture amount exceeds estimated hold amount")
	}
	payerID, err := uuid.Parse(hold.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("invalid hold customer_id")
	}
	payer := identity.CustomerID(payerID)
	trx, err := s.moneyService().CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  hold.Invoker,
		Currency: hold.Currency,
		Amount:   req.Amount,
		Source:   hold.Source,
		SourceID: hold.RequestID,
	})
	if err != nil {
		return nil, err
	}
	if _, rerr := store.Release(ctx, mid, req.RequestID); rerr != nil {
		log.Warnf("service capture: redis hold release failed after capture (request_id %s): %v", req.RequestID, rerr)
	}
	// #473: settle ALL composed budget-scope reservations for this request to
	// "captured" by the hold's (payer, source, source_id) coords.
	s.captureBudgetScopesByCoords(ctx, payer, hold.Currency, hold.Source, hold.RequestID, req.Amount)
	// #311: append an analytics usage_event linked to this capture (no second
	// debit) so OpenRails is the source of truth for platform usage/revenue.
	if strings.TrimSpace(req.EventType) != "" {
		source := strings.TrimSpace(req.Source)
		if source == "" {
			source = strings.TrimSpace(hold.Source)
		}
		sourceID := strings.TrimSpace(req.SourceID)
		if sourceID == "" {
			sourceID = strings.TrimSpace(hold.RequestID)
		}
		resource := strings.TrimSpace(req.Resource)
		if resource == "" {
			resource = strings.TrimSpace(hold.Resource)
		}
		captureTxnID := trx.ID
		if uerr := s.moneyService().InsertCaptureUsageEvent(ctx, money.CaptureUsageEventParams{
			CustomerID:         trx.CustomerID,
			Invoker:            trx.Invoker,
			Currency:           trx.Currency,
			EventType:          req.EventType,
			Resource:           resource,
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
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        trx.Currency,
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
	Currency    string `json:"currency"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount"`
}

// ServiceUsageRollupRequest selects a payer + window + grouping dimension.
type ServiceUsageRollupRequest struct {
	CustomerID *identity.CustomerID
	Currency   string
	From       time.Time
	To         time.Time
	GroupBy    string // endpoint | function | tier | user
}

// ServiceUsageRollup returns per-dimension-value spend for a payer over a
// window (#311) — the OpenRails-sourced data behind the platform's
// /budget-usage + revenue analytics. Service-scoped (operator service token).
func (s *Service) ServiceUsageRollup(ctx context.Context, req ServiceUsageRollupRequest) ([]ServiceUsageRollupRow, error) {
	if req.CustomerID == nil || req.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	rows, err := s.moneyService().ServiceUsageRollup(ctx, *req.CustomerID, currency, req.From, req.To, req.GroupBy)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceUsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServiceUsageRollupRow{Key: r.Key, Currency: r.Currency, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
	}
	return out, nil
}

// ResourceRevenueDailyRow is one day's revenue in internal units for an endpoint.
type ResourceRevenueDailyRow struct {
	Date     string `json:"date"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// ResourceRevenueDaily returns per-day revenue for a resource (typed
// attribution column) across all payers in the merchant over [from, to) — powers
// tensorhub endpoint revenue analytics (#410).
func (s *Service) ResourceRevenueDaily(ctx context.Context, resource, currency string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	rows, err := s.moneyService().ResourceRevenueDaily(ctx, resource, currency, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceRevenueDailyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ResourceRevenueDailyRow{Date: r.Date, Currency: r.Currency, Amount: r.Amount})
	}
	return out, nil
}

func (s *Service) ReleaseHold(ctx context.Context, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("request_id required")
	}
	store, err := s.holdStore()
	if err != nil {
		return err
	}
	mid, err := serviceMerchantID(ctx)
	if err != nil {
		return err
	}
	hold, err := store.Release(ctx, mid, requestID)
	if err != nil {
		return err
	}
	payerID, err := uuid.Parse(hold.CustomerID)
	if err != nil {
		return fmt.Errorf("invalid hold customer_id")
	}
	payer := identity.CustomerID(payerID)
	// #473: free ALL composed budget-scope reservations for this request (every
	// scope reserved under the hold's (payer, source, source_id)). Idempotent.
	s.releaseBudgetScopesByCoords(ctx, payer, hold.Source, hold.RequestID)
	return nil
}

func (s *Service) holdStore() (*holds.Store, error) {
	if s == nil || s.rt == nil || s.rt.RedisClient == nil {
		return nil, fmt.Errorf("hold store unavailable: redis not configured")
	}
	return holds.NewStore(s.rt.RedisClient), nil
}

func serviceMerchantID(ctx context.Context) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return mid.UUID().String(), nil
}

// releaseBudgetScopes settles every budget-scope reservation for a released
// hold to "released" by its (payer, source, source_id) coords (#473). Budget
// settlement is best-effort: the money hold is already released, so a budget
// settle failure must not fail the release (a stale active budget row self-heals
// at its reservation TTL).
func (s *Service) releaseBudgetScopesByCoords(ctx context.Context, payer identity.CustomerID, source, sourceID string) {
	sourceID = strings.TrimSpace(sourceID)
	if payer.IsZero() || sourceID == "" {
		return
	}
	bsvc := budgets.NewService(s.rt.DB)
	for _, cur := range money.CurrencyCodes() {
		if err := bsvc.ReleaseByCoords(ctx, payer, cur, source, sourceID); err != nil {
			log.Warnf("service release: budget scope release failed (request_id %s released, currency %s): %v", sourceID, cur, err)
		}
	}
}

// captureBudgetScopes settles every budget-scope reservation for a captured
// hold to "captured" with the captured amount by its (payer, source, source_id)
// coords (#473). Best-effort, mirroring releaseBudgetScopes.
func (s *Service) captureBudgetScopesByCoords(ctx context.Context, payer identity.CustomerID, currency, source, sourceID string, capturedAmount int64) {
	sourceID = strings.TrimSpace(sourceID)
	if payer.IsZero() || sourceID == "" {
		return
	}
	bsvc := budgets.NewService(s.rt.DB)
	for _, cur := range money.CurrencyCodes() {
		var err error
		if cur == money.NormalizeCurrency(currency) {
			err = bsvc.CaptureByCoords(ctx, payer, cur, source, sourceID, capturedAmount)
		} else {
			err = bsvc.CaptureByCoordsReservedAmount(ctx, payer, cur, source, sourceID)
		}
		if err != nil {
			log.Warnf("service capture: budget scope capture failed (request_id %s captured, currency %s): %v", sourceID, cur, err)
		}
	}
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

func (s *Service) ListActiveEntitlementsForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, at time.Time) ([]string, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlementsByCustomer(ctx, tenantSubjectID.UUID(), at.UTC())
}

func (s *Service) IsCustomerEntitled(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (bool, error) {
	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("customer_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().IsCustomerEntitled(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

func (s *Service) HasActiveIndefiniteEntitlementForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (bool, error) {
	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("customer_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().HasActiveIndefiniteByCustomer(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

func (s *Service) LatestFiniteEntitlementWindowForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (*EntitlementRecord, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return nil, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	ent, err := s.entitlementService().LatestFiniteWindowByCustomer(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
	if err != nil {
		return nil, err
	}
	record := entitlementRecordFromEntitlement(ent)
	return &record, nil
}

type EntitlementRecord struct {
	ID           uuid.UUID
	CustomerID   uuid.UUID
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
			ID:           e.ID,
			CustomerID:   e.CustomerID,
			UserID:       e.CustomerID.String(),
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

func (s *Service) ListActiveEntitlementRecordsForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, at time.Time) ([]EntitlementRecord, error) {
	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	records, err := s.entitlementService().ListActiveRecordsByCustomer(ctx, tenantSubjectID.UUID(), at.UTC())
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
			CustomerID:   e.CustomerID,
			UserID:       e.CustomerID.String(),
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

// EntitlementsBatchMaxSubjects bounds one batch read (#354); over-cap is an
// explicit error at the edge, never a silent truncation. Shared by the
// standalone handler and the embedded transport.
const EntitlementsBatchMaxSubjects = 500

// ListActiveEntitlementRecordsByExternalSubjects (#354): active rows for many
// external subjects of one (ambient-merchant, issuer) in one query, grouped by
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
				ID:           e.ID,
				CustomerID:   e.CustomerID,
				UserID:       e.CustomerID.String(),
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
		out[subject] = rs
	}
	return out, nil
}
