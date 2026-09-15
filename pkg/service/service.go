package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
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
// made a blank echo a second debit.
const captureSourceNamespace = "admit"

type CaptureHoldRequest struct {
	// RequestID is the caller's idempotency key for this capture (or#907): the
	// admit's request id. The durable ledger coordinate is composed from it and
	// engine constants alone — no volatile state and no caller-echoed field
	// participates — so ANY retry of the same request dedupes, unconditionally.
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
	// (opaque; an endpoint slug is one example). Optional.
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
	currency, err := s.resolveCurrency(ctx, req.Currency)
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
	displayCurrency, displayErr := s.DisplayCurrency(ctx, trx.Currency)
	if displayErr != nil {
		return nil, displayErr
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        displayCurrency,
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
// amount, currency or expiry differs is refused with ErrIdempotencyKeyReused.
// Diagnostic source/invoker/description changes retain the original receipt.
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
	currency, err := s.resolveCurrency(ctx, req.Currency)
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
	displayCurrency, displayErr := s.DisplayCurrency(ctx, trx.Currency)
	if displayErr != nil {
		return nil, displayErr
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        displayCurrency,
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
	displayCurrency, displayErr := s.DisplayCurrency(ctx, trx.Currency)
	if displayErr != nil {
		return nil, displayErr
	}
	return &CreditTransaction{
		ID:              trx.ID,
		CustomerID:      trx.CustomerID,
		Invoker:         trx.Invoker,
		Currency:        displayCurrency,
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

func (s *Service) CaptureHold(ctx context.Context, req CaptureHoldRequest) (*openrails.CaptureReceipt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	captured, err := s.moneyService().CaptureAdmission(ctx, req.RequestID, req.Amount)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.EventType) != "" {
		usageSource := strings.TrimSpace(req.Source)
		if usageSource == "" {
			usageSource = captureSourceNamespace
		}
		sourceID := strings.TrimSpace(req.SourceID)
		if sourceID == "" {
			sourceID = req.RequestID
		}
		if err := s.moneyService().InsertCaptureUsageEvent(ctx, money.CaptureUsageEventParams{
			CustomerID: captured.CustomerID, Invoker: captured.Terms.Invoker, Currency: captured.Currency,
			EventType: req.EventType, Resource: strings.TrimSpace(req.Resource), Amount: req.Amount,
			Dimensions: req.Dimensions, Metadata: req.Metadata, Source: usageSource, SourceID: sourceID,
			LedgerTransferID: captured.LedgerTransferID,
		}); err != nil {
			log.Warnf("capture usage event failed for request %s: %v", req.RequestID, err)
		}
	}
	return captured.CaptureReceipt, nil
}

// AdmissionCustomer resolves ownership from the durable operation before route authorization.
func (s *Service) AdmissionCustomer(ctx context.Context, requestID string) (identity.CustomerID, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return identity.CustomerID{}, err
	}
	defer release()
	row, err := spendgate.New(s.rt.DB).Get(ctx, requestID)
	return identity.CustomerID(row.PayerID), err
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
	currency, err := s.resolveCurrency(ctx, req.Currency)
	if err != nil {
		return nil, err
	}
	rows, err := s.moneyService().ServiceUsageRollup(ctx, *req.CustomerID, currency, req.From, req.To, req.GroupBy)
	if err != nil {
		return nil, err
	}
	displayCurrency, err := s.DisplayCurrency(ctx, currency)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceUsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServiceUsageRollupRow{Key: r.Key, Currency: displayCurrency, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
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
// endpoint revenue analytics (#410).
func (s *Service) ResourceRevenueDaily(ctx context.Context, resource, currency string, from, to time.Time) ([]ResourceRevenueDailyRow, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	currency, err := s.resolveCurrency(ctx, currency)
	if err != nil {
		return nil, err
	}
	rows, err := s.moneyService().ResourceRevenueDaily(ctx, resource, currency, from, to)
	if err != nil {
		return nil, err
	}
	displayCurrency, err := s.DisplayCurrency(ctx, currency)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceRevenueDailyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ResourceRevenueDailyRow{Date: r.Date, Currency: displayCurrency, Amount: r.Amount})
	}
	return out, nil
}

func (s *Service) ReleaseHold(ctx context.Context, requestID string) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("request_id required")
	}
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	return gate.Release(ctx, strings.TrimSpace(requestID))
}

func (s *Service) ExtendHold(ctx context.Context, requestID string, expiresAt time.Time) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("request_id required")
	}
	if expiresAt.IsZero() {
		return ErrHoldDeadlineRequired
	}
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	err = gate.Extend(ctx, strings.TrimSpace(requestID), expiresAt)
	if errors.Is(err, spendgate.ErrExpired) {
		return ErrHoldDeadlinePassed
	}
	if errors.Is(err, spendgate.ErrNotFound) {
		return ErrHoldNotFound
	}
	return err
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
