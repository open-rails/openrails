package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// serviceIdempotencyConflict answers a reused-idempotency-key refusal with 409
// and the money layer's own detail message ("idempotency_key_reused: <op>
// replayed key (…) with amount=… but that key already committed amount=…").
// That is the ONE wire shape both SDK transports map back to
// openrails.ErrIdempotencyKeyReused (errors.go), and pkg/service re-exports the
// engine-side twin. It is a CALLER bug — the retry changed the charging terms —
// so it must never be answered 500, which is indistinguishable from an engine
// fault and invites a retry that will refuse again.
func serviceIdempotencyConflict(r *httprequest.Request, err error) bool {
	if !errors.Is(err, billingservice.ErrIdempotencyKeyReused) {
		return false
	}
	r.ErrorJSON(http.StatusConflict, err.Error())
	return true
}

type serviceDepositRequest struct {
	CustomerID string `json:"customer_id"`
	Invoker    string `json:"invoker"`
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount" binding:"required"`
	Source     string `json:"source" binding:"required"`
	// SourceID is the caller's reproducible idempotency key (or#906: any
	// non-empty string, no longer restricted to a UUID — the structural key
	// openrails.uq_grants_credit_deposit_once enforces is text).
	SourceID    string  `json:"source_id" binding:"required"`
	ExpiresAt   *int64  `json:"expires_at"`
	Description *string `json:"description"`
}

type serviceCaptureRequest struct {
	Amount int64 `json:"amount" binding:"required"`

	// Fallback payer coordinates (#676, additive): used only when the admit-time
	// Redis pointer is gone, so a rendered service stays chargeable. The
	// idempotency coordinate is the path request_id alone (or#907) — there is no
	// admit_source echo any more; a retry dedupes regardless of what the admit
	// was placed with.
	CustomerID string `json:"customer_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
	Invoker    string `json:"invoker,omitempty"`

	// Usage analytics (#311): when event_type is set, the capture also appends a
	// usage_event (no second debit) for the platform usage/revenue rollup.
	EventType  string           `json:"event_type,omitempty"`
	Resource   string           `json:"resource,omitempty"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source,omitempty"`
	SourceID   string           `json:"source_id,omitempty"`
}

func parseServiceCustomerID(raw string) (*billingidentity.CustomerID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return nil, errors.New("invalid customer_id")
	}
	tenantSubject := billingidentity.CustomerID(id)
	return &tenantSubject, nil
}

// serviceCredentialFromRequest returns the resolved service credential the auth
// middleware pinned onto the context, or a (status, message) error pair.
func serviceCredentialFromRequest(r *httprequest.Request) (*controlplane.ResolvedServiceCredential, int, string) {
	v, ok := r.Get(middleware.ServiceCredentialContextKey)
	if !ok {
		return nil, http.StatusUnauthorized, "API key required"
	}
	resolved, ok := v.(*controlplane.ResolvedServiceCredential)
	if !ok || resolved == nil {
		return nil, http.StatusInternalServerError, "API key state invalid"
	}
	return resolved, 0, ""
}

const MerchantRoutePrincipalContextKey = "openrails.merchant_route_principal"

func hasMerchantRoutePrincipal(r *httprequest.Request) bool {
	if r == nil {
		return false
	}
	if _, ok := r.Get(MerchantRoutePrincipalContextKey); ok {
		return true
	}
	return false
}

func requireServiceCustomerScope(r *httprequest.Request, tenantSubject billingidentity.CustomerID) bool {
	if v, ok := r.Get(middleware.ServiceCredentialContextKey); ok {
		resolved, ok := v.(*controlplane.ResolvedServiceCredential)
		if !ok || resolved == nil {
			r.ErrorJSON(http.StatusInternalServerError, "API key state invalid")
			return false
		}
		if !resolved.AllowsCustomer(tenantSubject.UUID()) {
			r.ErrorJSON(http.StatusForbidden, "service_credential_customer_scope_denied")
			return false
		}
		return true
	}
	if hasMerchantRoutePrincipal(r) {
		return true
	}
	r.ErrorJSON(http.StatusUnauthorized, "merchant principal required")
	return false
}

func serviceCustomerScopeAllows(r *httprequest.Request, tenantSubject billingidentity.CustomerID) bool {
	if v, ok := r.Get(middleware.ServiceCredentialContextKey); ok {
		resolved, ok := v.(*controlplane.ResolvedServiceCredential)
		return ok && resolved != nil && resolved.AllowsCustomer(tenantSubject.UUID())
	}
	return hasMerchantRoutePrincipal(r)
}

func requireMerchantRoutePrincipal(r *httprequest.Request) bool {
	if hasMerchantRoutePrincipal(r) {
		return true
	}
	if resolved, status, msg := serviceCredentialFromRequest(r); resolved == nil {
		r.ErrorJSON(status, msg)
		return false
	}
	return true
}

// serviceBalanceResponse is the customer balance snapshot served by
// GET /v1/merchant/credits/balance (issue #235/#247).
type serviceBalanceResponse struct {
	CustomerID            uuid.UUID `json:"customer_id"`
	Currency              string    `json:"currency"`
	BillingMode           string    `json:"billing_mode"`
	BalanceAmount         int64     `json:"balance_amount"`
	HeldAmount            int64     `json:"held_amount"`
	AvailableAmount       int64     `json:"available_amount"`
	OutstandingOwedAmount int64     `json:"outstanding_owed_amount"`
}

// ServiceGetCreditsBalance returns the customer's REAL balance snapshot (issue
// #235/#247): available = balance - held, plus outstanding owed + billing mode.
// Merchant-bound by the API key (RLS); customer supplied via ?customer_id=.
func ServiceGetCreditsBalance(r *httprequest.Request) {
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}
	tenantSubject, err := parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	if tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubject) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	snap, err := svc.GetCreditAccount(r.Request.Context(), *tenantSubject, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(serviceBalanceResponse{
		CustomerID:            snap.CustomerID,
		Currency:              snap.Currency,
		BillingMode:           snap.BillingMode,
		BalanceAmount:         snap.BalanceAmount,
		HeldAmount:            snap.HeldAmount,
		AvailableAmount:       snap.AvailableAmount,
		OutstandingOwedAmount: snap.OutstandingOwedAmount,
	})
}

func serviceRequiredCurrency(r *httprequest.Request, raw string) (string, bool) {
	currency := strings.TrimSpace(raw)
	if currency == "" {
		r.ErrorJSON(http.StatusBadRequest, "currency required")
		return "", false
	}
	if money.IsQualifiedUnit(currency) {
		return currency, true
	}
	return money.NormalizeCurrency(currency), true
}

type serviceTxnResponse struct {
	ID              uuid.UUID `json:"id"`
	CustomerID      uuid.UUID `json:"customer_id"`
	Invoker         string    `json:"invoker"`
	Currency        string    `json:"currency"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

type serviceUsageRollupRequest struct {
	CustomerID string `json:"customer_id" binding:"required"`
	Currency   string `json:"currency"`
	From       int64  `json:"from" binding:"required"` // unix seconds, inclusive
	To         int64  `json:"to" binding:"required"`   // unix seconds, exclusive
	GroupBy    string `json:"group_by" binding:"required"`
}

type serviceRecordUsageRequest struct {
	CustomerID     string           `json:"customer_id" binding:"required"`
	Invoker        string           `json:"invoker"`
	Currency       string           `json:"currency"`
	EventType      string           `json:"event_type" binding:"required"`
	Dimensions     map[string]int64 `json:"dimensions"`
	Amount         int64            `json:"amount"`
	Resource       string           `json:"resource"`
	Metadata       map[string]any   `json:"metadata"`
	Source         string           `json:"source" binding:"required"`
	SourceID       string           `json:"source_id" binding:"required"`
	OccurredAtUnix int64            `json:"occurred_at_unix"`
}

// ServiceRecordUsage records one host-reported metered usage event (#797): a
// usage_events row (plus a ledger debit for a non-zero amount) that the
// rate-card rating sweep aggregates into arrears invoice lines. Idempotent on
// (payer, event_type, source, source_id). Operator API key, admissions:create.
func ServiceRecordUsage(r *httprequest.Request) {
	var req serviceRecordUsageRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.Amount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount must be >= 0")
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	var occurredAt time.Time
	if req.OccurredAtUnix > 0 {
		occurredAt = time.Unix(req.OccurredAtUnix, 0).UTC()
	}
	usageKey, err := money.NewIdempotencyKey(money.UsageOperation(req.EventType), req.Source, req.SourceID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if err := svc.RecordUsage(r.Request.Context(), billingservice.RecordUsageInput{
		CustomerID: *payer,
		Invoker:    req.Invoker,
		Currency:   currency,
		EventType:  req.EventType,
		Dimensions: req.Dimensions,
		Amount:     req.Amount,
		Resource:   req.Resource,
		Metadata:   req.Metadata,
		Key:        usageKey,
		OccurredAt: occurredAt,
	}); err != nil {
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "recorded": true})
}

type serviceEndpointRevenueRequest struct {
	Resource string `json:"resource" binding:"required"`
	Currency string `json:"currency"`
	From     int64  `json:"from" binding:"required"`
	To       int64  `json:"to" binding:"required"`
}

// ServiceResourceRevenue returns per-day revenue for a resource (by usage_event
// the typed resource column) across all customers in the merchant (#410) — powers
// tensorhub endpoint revenue analytics. Operator API key, credits:read.
func ServiceResourceRevenue(r *httprequest.Request) {
	var req serviceEndpointRevenueRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	rows, err := svc.ResourceRevenueDaily(r.Request.Context(), req.Resource, currency, time.Unix(req.From, 0).UTC(), time.Unix(req.To, 0).UTC())
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var total int64
	for _, x := range rows {
		total += x.Amount
	}
	r.SuccessJSON(map[string]any{"currency": currency, "revenue_amount": total, "daily": rows})
}

// ServiceUsageRollup returns per-dimension-value spend for a customer over a
// window (#311) — the OpenRails-sourced data behind the tensorhub platform's
// /budget-usage + revenue analytics. Operator API key, credits:read scope.
func ServiceUsageRollup(r *httprequest.Request) {
	var req serviceUsageRollupRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	tenantSubjectID, err := parseServiceCustomerID(req.CustomerID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubjectID) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	rows, err := svc.ServiceUsageRollup(r.Request.Context(), billingservice.ServiceUsageRollupRequest{
		CustomerID: tenantSubjectID,
		Currency:   currency,
		From:       time.Unix(req.From, 0).UTC(),
		To:         time.Unix(req.To, 0).UTC(),
		GroupBy:    req.GroupBy,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "rows": rows})
}

func ServiceDepositCredits(r *httprequest.Request) {
	var req serviceDepositRequest
	if !r.BindJSON(&req) {
		return
	}
	invoker := strings.TrimSpace(req.Invoker)
	if invoker == "" {
		r.ErrorJSON(http.StatusBadRequest, "invoker required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	tenantSubjectID, err := parseServiceCustomerID(req.CustomerID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubjectID) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}

	depositKey, err := billingservice.NewDepositIdempotencyKey(req.Source, req.SourceID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		CustomerID:  tenantSubjectID,
		Invoker:     invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Key:         depositKey,
		ExpiresAt:   expiresAt,
		Description: req.Description,
	})
	if err != nil {
		// #483: an unknown/invalid currency is a client error (parity with local), not a 500.
		if errors.Is(err, money.ErrBillingUnitRequired) || strings.Contains(err.Error(), "unknown currency") {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "deposit failed")
		return
	}
	r.SuccessJSON(trx)
}

// adminGrantCreditsRequest is the or#906 human-admin credit grant body.
// source_id is the caller's REPRODUCIBLE idempotency key (the deposit's
// structural identity — unique per (merchant, customer) in the database);
// source is a descriptive label and deliberately NOT part of the key.
type adminGrantCreditsRequest struct {
	Invoker     string  `json:"invoker"`
	Currency    string  `json:"currency"`
	Amount      int64   `json:"amount" binding:"required"`
	Source      string  `json:"source"`
	SourceID    string  `json:"source_id" binding:"required"`
	ExpiresAt   *int64  `json:"expires_at"`
	Description *string `json:"description"`
}

// AdminGrantCredits is POST /v1/merchant/customers/{customer_id}/credits
// (or#906): the human-admin sibling of the entitlement/product-access grants,
// gated merchant:credits:grant + the AdminOperationGrant velocity limit. It is
// the SAME deposit as the machine route — same idempotency key, same 409 on a
// changed-amount replay, same Replayed reporting — addressed at a customer.
func AdminGrantCredits(r *httprequest.Request) {
	tenantSubjectID, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	var req adminGrantCreditsRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.Amount <= 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount must be > 0")
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "admin"
	}
	invoker := strings.TrimSpace(req.Invoker)
	if invoker == "" {
		invoker = tenantSubjectID.String()
	}
	depositKey, err := billingservice.NewDepositIdempotencyKey(source, req.SourceID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}
	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		CustomerID:  tenantSubjectID,
		Invoker:     invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Key:         depositKey,
		ExpiresAt:   expiresAt,
		Description: req.Description,
	})
	if err != nil {
		if errors.Is(err, money.ErrBillingUnitRequired) || strings.Contains(err.Error(), "unknown currency") {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "credit grant failed")
		return
	}
	r.SuccessJSON(trx)
}

// ServiceGetDeposit is GET /v1/merchant/credits/deposit (or#906): the
// key-qualified "what did this key do" read. Query: customer_id + source_id —
// the deposit's full structural key; operation is fixed to deposit by the
// route, so the coordinate is unambiguous (or#894's keyless read stays
// deleted). Answers the committed grant as the same CreditTransaction shape a
// replay POST returns (Replayed=true), or 404 when the key never committed.
func ServiceGetDeposit(r *httprequest.Request) {
	tenantSubjectID, err := parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	sourceID := strings.TrimSpace(r.Request.URL.Query().Get("source_id"))
	if sourceID == "" {
		r.ErrorJSON(http.StatusBadRequest, "source_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubjectID) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.GetDeposit(r.Request.Context(), *tenantSubjectID, sourceID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "deposit lookup failed")
		return
	}
	if trx == nil {
		r.ErrorJSON(http.StatusNotFound, "deposit_not_found")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceCaptureHold(r *httprequest.Request) {
	requestID := strings.TrimSpace(r.Param("id"))
	if requestID == "" {
		r.ErrorJSON(http.StatusBadRequest, "request_id required")
		return
	}
	var req serviceCaptureRequest
	if !r.BindJSON(&req) {
		return
	}
	// #676: a supplied fallback customer_id must pass the credential's customer
	// scope (parity with deposit/withdraw).
	if fbCustomer, err := parseServiceCustomerID(req.CustomerID); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	} else if fbCustomer != nil && !requireServiceCustomerScope(r, *fbCustomer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{
		RequestID:  requestID,
		Amount:     req.Amount,
		CustomerID: req.CustomerID,
		Currency:   req.Currency,
		Invoker:    req.Invoker,
		EventType:  req.EventType,
		Resource:   req.Resource,
		Dimensions: req.Dimensions,
		Metadata:   req.Metadata,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "capture failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceReleaseHold(r *httprequest.Request) {
	requestID := strings.TrimSpace(r.Param("id"))
	if requestID == "" {
		r.ErrorJSON(http.StatusBadRequest, "request_id required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.ReleaseHold(r.Request.Context(), requestID); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "release failed")
		return
	}
	r.SuccessJSON(map[string]any{"ok": true})
}

type serviceExtendHoldRequest struct {
	ExpiresAt int64 `json:"expires_at"`
}

// ServiceExtendHold re-declares a live hold's deadline (xs-007 row 33).
func ServiceExtendHold(r *httprequest.Request) {
	requestID := strings.TrimSpace(r.Param("id"))
	if requestID == "" {
		r.ErrorJSON(http.StatusBadRequest, "request_id required")
		return
	}
	var req serviceExtendHoldRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.ExpiresAt <= 0 {
		r.ErrorJSON(http.StatusBadRequest, "expires_at required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	err = svc.ExtendHold(r.Request.Context(), requestID, time.Unix(req.ExpiresAt, 0).UTC())
	switch {
	case err == nil:
		r.SuccessJSON(map[string]any{"ok": true})
	case errors.Is(err, billingservice.ErrHoldNotFound):
		r.ErrorJSON(http.StatusNotFound, "hold_not_found")
	case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
		r.ErrorJSON(http.StatusBadRequest, "expires_at already passed")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "extend failed")
	}
}
