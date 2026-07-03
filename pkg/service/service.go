package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
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

type CaptureHoldRequest struct {
	RequestID string
	Amount    int64

	// Fallback payer coordinates (#676). The admit-time request→payer pointer
	// lives in Redis and can be lost (flush/failover/TTL overrun). When the
	// pointer resolve misses AND CustomerID+Currency are supplied, capture
	// proceeds against them — a rendered service is always chargeable.
	// AdmitSource must echo the Source the admit was placed with (defaults to
	// "admit", the Admit default) so the durable idempotency coordinates match a
	// pointer-resolved capture of the same request. All ignored when the pointer
	// is live.
	CustomerID  string
	Currency    string
	Invoker     string
	AdmitSource string

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
	if s.rt == nil || s.rt.RedisClient == nil {
		return nil, fmt.Errorf("capture unavailable: redis not configured")
	}
	mid, err := serviceMerchantID(ctx)
	if err != nil {
		return nil, err
	}
	// Resolve the payer coords the admit-time hold was placed under (#513); on a
	// miss (or Redis failure) fall back to caller-supplied coordinates (#676) so
	// capture never depends on volatile state alone.
	gate := spendgate.New(s.rt.RedisClient)
	ref, ok, rerr := gate.Resolve(ctx, mid, req.RequestID)
	if !ok || rerr != nil {
		fbCustomer := strings.TrimSpace(req.CustomerID)
		fbCurrency := strings.TrimSpace(req.Currency)
		if fbCustomer == "" || fbCurrency == "" {
			if rerr != nil {
				return nil, rerr
			}
			return nil, fmt.Errorf("hold not found for request_id %q", req.RequestID)
		}
		ref = spendgate.HoldRef{
			Customer: fbCustomer,
			Currency: fbCurrency,
			Invoker:  strings.TrimSpace(req.Invoker),
			Source:   strings.TrimSpace(req.AdmitSource),
		}
		if ref.Source == "" {
			ref.Source = "admit" // Admit's default source namespace
		}
	}
	payerID, err := uuid.Parse(ref.Customer)
	if err != nil {
		return nil, fmt.Errorf("invalid hold customer_id")
	}
	payer := identity.CustomerID(payerID)
	source := ref.Source
	if source == "" {
		source = "usage"
	}
	// Durable money movement (#512 ledger), idempotent on
	// (merchant, payer, currency, source, source_id) = the admit request id.
	trx, err := s.moneyService().CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  ref.Invoker,
		Currency: ref.Currency,
		Amount:   req.Amount,
		Source:   source,
		SourceID: req.RequestID,
	})
	if err != nil {
		return nil, err
	}
	// Free the Redis reservation (estimate-based: windows keep the estimate). The
	// durable charge already landed, so a release failure only inflates `held`
	// until its TTL/sweep — never a lost/double charge.
	if cerr := gate.Capture(ctx, spendgate.CaptureInput{Merchant: mid, Customer: ref.Customer, Currency: ref.Currency, RequestID: req.RequestID}); cerr != nil {
		log.Warnf("service capture: spendgate release failed after durable capture (request_id %s): %v", req.RequestID, cerr)
	}
	// #311: append an analytics usage_event linked to this capture (no second
	// debit) so OpenRails is the source of truth for platform usage/revenue.
	if strings.TrimSpace(req.EventType) != "" {
		usageSource := strings.TrimSpace(req.Source)
		if usageSource == "" {
			usageSource = source
		}
		sourceID := strings.TrimSpace(req.SourceID)
		if sourceID == "" {
			sourceID = req.RequestID
		}
		resource := strings.TrimSpace(req.Resource)
		captureTxnID := trx.ID
		if uerr := s.moneyService().InsertCaptureUsageEvent(ctx, money.CaptureUsageEventParams{
			CustomerID:       trx.CustomerID,
			Invoker:          trx.Invoker,
			Currency:         trx.Currency,
			EventType:        req.EventType,
			Resource:         resource,
			Amount:           req.Amount,
			Dimensions:       req.Dimensions,
			Metadata:         req.Metadata,
			Source:           usageSource,
			SourceID:         sourceID,
			LedgerTransferID: &captureTxnID,
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
// /budget-usage + revenue analytics. Service-scoped (operator API key).
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
	if s.rt == nil || s.rt.RedisClient == nil {
		return fmt.Errorf("release unavailable: redis not configured")
	}
	mid, err := serviceMerchantID(ctx)
	if err != nil {
		return err
	}
	gate := spendgate.New(s.rt.RedisClient)
	ref, ok, err := gate.Resolve(ctx, mid, requestID)
	if err != nil {
		return err
	}
	if !ok {
		// Already settled / never held / TTL-expired — idempotent no-op.
		return nil
	}
	// Free the in-flight reservation AND the spend-cap window estimates (the request
	// did not happen). Idempotent.
	return gate.Release(ctx, spendgate.ReleaseInput{Merchant: mid, Customer: ref.Customer, Currency: ref.Currency, RequestID: requestID})
}

func serviceMerchantID(ctx context.Context) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return mid.UUID().String(), nil
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
// ListCustomersWithEntitlement is the reverse lookup (#535): customer ids holding
// an active window of `entitlement` for the merchant, keyset-paginated (afterID
// exclusive; uuid.Nil starts). A zero `at` means now.
func (s *Service) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time, afterID uuid.UUID, limit int) ([]uuid.UUID, error) {
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListCustomersWithEntitlement(ctx, entitlement, at.UTC(), afterID, limit)
}

func (s *Service) ListActiveEntitlementRecordsByExternalSubjects(ctx context.Context, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	if len(subjects) == 0 {
		return map[string][]EntitlementRecord{}, nil
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	grouped, err := s.entitlementService().ListActiveRecordsByExternalSubjects(ctx, subjects, at.UTC())
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
