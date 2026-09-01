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

// ErrIdempotencyKeyReused is the money-write refusal a caller can act on: the
// key already committed, and THIS retry carries different charging terms
// (or#891). It is re-exported here because the rule lives in internal/ — a host
// could see the failure but not name it, so it could not tell a caller bug
// apart from an engine fault. Wrapped errors carry the detail
// (money.IdempotencyConflict: which field, committed vs retried).
var ErrIdempotencyKeyReused = money.ErrIdempotencyKeyReused

// captureSourceNamespace is the FIXED source half of every capture's ledger
// coordinate (or#907). It matches Admit's default source namespace, so capture
// coordinates written before or#907 for default-source admits are identical.
// It is deliberately NOT the admit-time source: that lived in the Redis hold
// ref the first capture consumes, and rebuilding it from a caller echo is what
// made a blank echo a second debit (tensorhub th#969's outbox existed to
// compensate).
const captureSourceNamespace = "admit"

type CaptureHoldRequest struct {
	// RequestID is the caller's idempotency key for this capture (or#907): the
	// admit's request id. The durable ledger coordinate is composed from it and
	// engine constants alone — no volatile state and no caller-echoed field
	// participates — so ANY retry of the same request dedupes, unconditionally.
	RequestID string
	Amount    int64

	// Fallback payer coordinates (#676). The admit-time request→payer pointer
	// lives in Redis and can be lost (flush/failover/TTL overrun). When the
	// pointer resolve misses AND CustomerID+Currency are supplied, capture
	// proceeds against them — a rendered service is always chargeable. All
	// ignored when the pointer is live.
	CustomerID string
	Currency   string
	Invoker    string

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
	// Replayed reports that this write's idempotency key had ALREADY committed,
	// so nothing moved in this call — the transaction described here is the
	// movement that landed earlier (or#892). This is the applied-vs-replayed
	// answer consumers were building their own claim tables to get.
	Replayed bool
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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
		Replayed:        trx.Replayed,
	}, nil
}

// DepositIdempotencyKey is the reproducible coordinate for one credit deposit
// (or#906, mirroring UsageIdempotencyKey from 704f0bd0).
type DepositIdempotencyKey = money.IdempotencyKey

// NewDepositIdempotencyKey builds a deposit key (operation bound to deposit).
// source labels the system of record ("stripe", "admin", …); sourceID is the
// caller's reproducible key and is the deposit's structural identity — the
// database enforces once-only at (merchant, customer, sourceID), so the SAME
// sourceID under a DIFFERENT source is still the same deposit (doctrine, see
// client.go DepositCreditsRequest.SourceID).
func NewDepositIdempotencyKey(source, sourceID string) (DepositIdempotencyKey, error) {
	return money.NewIdempotencyKey(money.OpDeposit, source, sourceID)
}

// DepositCreditsRequest is one admin/rail credit deposit. Key is the deposit's
// idempotency coordinate (operation=deposit); build it with
// NewDepositIdempotencyKey(source, sourceID). It must be REPRODUCIBLE across
// retries of the same logical deposit — a value minted per attempt passes
// every check and double-credits.
//
// The deposit's structural key is the credit grant's (merchant, customer,
// source_id), UNIQUE in the database (or#906 migration 0004). An identical
// replay is answered with the EXISTING grant (Replayed=true); a replay whose
// Amount differs from what the key committed is refused with
// ErrIdempotencyKeyReused (amount only — currency/expiry/description drift is
// tolerated, the or#891 posture everywhere).
type DepositCreditsRequest struct {
	CustomerID  *identity.CustomerID
	Invoker     string
	Currency    string
	Amount      int64
	Key         money.IdempotencyKey
	ExpiresAt   *time.Time
	Description *string
}

func (s *Service) DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	req.Invoker = strings.TrimSpace(req.Invoker)
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
	if err := req.Key.RequireOperation(money.OpDeposit); err != nil {
		return nil, err
	}
	depositSourceID := req.Key.SourceID()
	trx, err := s.moneyService().Deposit(ctx, money.DepositParams{
		CustomerID:  req.CustomerID,
		Invoker:     req.Invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Key.Source(),
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
		Replayed:        trx.Replayed,
	}, nil
}

// GetDeposit answers "what did this deposit key do" (or#906): the grant id,
// amount, and created_at committed at the caller's key, with Replayed=true —
// exactly what a replay POST would answer, without needing the amount. Returns
// (nil, nil) when the key never committed. sourceID is the deposit key's
// caller half; the operation half is fixed to deposit by this method
// (key-qualified — or#894 deleted the keyless coordinate read as a trap).
func (s *Service) GetDeposit(ctx context.Context, customerID identity.CustomerID, sourceID string) (*CreditTransaction, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if customerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	trx, err := s.moneyService().GetDepositBySourceID(ctx, customerID, sourceID)
	if err != nil {
		return nil, err
	}
	if trx == nil {
		return nil, nil
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        trx.Currency,
		Amount:          trx.Amount,
		TransactionType: trx.TransactionType,
		Status:          trx.Status,
		Source:          trx.Source,
		SourceID:        trx.SourceID,
		ExpiresAt:       trx.ExpiresAt,
		Description:     trx.Description,
		CreatedAt:       trx.CreatedAt,
		UpdatedAt:       trx.UpdatedAt,
		Replayed:        trx.Replayed,
	}, nil
}

func (s *Service) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
		}
	}
	payerID, err := uuid.Parse(ref.Customer)
	if err != nil {
		return nil, fmt.Errorf("invalid hold customer_id")
	}
	payer := identity.CustomerID(payerID)
	// Durable money movement (#512 ledger), idempotent on
	// (merchant, payer, currency, operation=capture, source="admit",
	// source_id=request id) — every part engine-composed or caller-key (or#907).
	// The source half is a CONSTANT on purpose: it used to be the admit-time
	// source recovered from the Redis hold ref, which the first capture
	// consumes, so a retry rebuilt it from a caller-echoed field and a blank
	// echo landed at a DIFFERENT coordinate — a second debit. A coordinate that
	// depends on nothing volatile is what makes capture idempotent on the
	// caller's key unconditionally. The operation is what keeps a capture from
	// aliasing a wasted-spend usage charge at the same request id (or#894).
	captureKey, err := money.NewIdempotencyKey(money.OpCapture, captureSourceNamespace, req.RequestID)
	if err != nil {
		return nil, err
	}
	trx, err := s.moneyService().CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  ref.Invoker,
		Currency: ref.Currency,
		Amount:   req.Amount,
		Key:      captureKey,
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
			usageSource = captureSourceNamespace
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
		Replayed:        trx.Replayed,
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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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

// ExtendHold re-declares the deadline of a live admission hold (xs-007 row
// 33): the job that owns request_id is still running and will now finish by
// expiresAt. Idempotent. ErrHoldNotFound when nothing live exists to extend.
func (s *Service) ExtendHold(ctx context.Context, requestID string, expiresAt time.Time) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("request_id required")
	}
	if expiresAt.IsZero() {
		return ErrHoldDeadlineRequired
	}
	if !expiresAt.After(s.now()) {
		return ErrHoldDeadlinePassed
	}
	if s.rt == nil || s.rt.RedisClient == nil {
		return fmt.Errorf("extend unavailable: redis not configured")
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
		return ErrHoldNotFound
	}
	extended, err := gate.Extend(ctx, spendgate.ExtendInput{
		Merchant: mid, Customer: ref.Customer, Currency: ref.Currency, RequestID: requestID, Until: expiresAt.UTC(),
	})
	if err != nil {
		return err
	}
	if !extended {
		return ErrHoldNotFound
	}
	return nil
}

func serviceMerchantID(ctx context.Context) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return mid.UUID().String(), nil
}

func (s *Service) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlementsByCustomer(ctx, tenantSubjectID.UUID(), at.UTC())
}

func (s *Service) IsCustomerEntitled(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (bool, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return false, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return false, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListCustomersWithEntitlement(ctx, entitlement, at.UTC(), afterID, limit)
}

func (s *Service) ListActiveEntitlementRecordsByExternalSubjects(ctx context.Context, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
