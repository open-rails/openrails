package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/repo"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceWithdrawRequest struct {
	CustomerID string     `json:"customer_id"`
	Actor      string     `json:"actor" binding:"required"`
	Amount     int64      `json:"amount" binding:"required"`
	Source     string     `json:"source" binding:"required"`
	SourceID   *uuid.UUID `json:"source_id" binding:"required"`
}

type serviceDepositRequest struct {
	CustomerID  string     `json:"customer_id"`
	Actor       string     `json:"actor" binding:"required"`
	Currency    string     `json:"currency"` // "" => DefaultCurrency (#476); #483: must forward so unknown units reject like local
	Amount      int64      `json:"amount" binding:"required"`
	Source      string     `json:"source" binding:"required"`
	SourceID    *uuid.UUID `json:"source_id" binding:"required"`
	ExpiresAt   *int64     `json:"expires_at"`
	Description *string    `json:"description"`
}

type serviceHoldRequest struct {
	CustomerID string `json:"customer_id"`
	Actor      string `json:"actor" binding:"required"`
	Amount     int64  `json:"amount" binding:"required"`
	Source     string `json:"source" binding:"required"`
	SourceID   string `json:"source_id" binding:"required"`
	ExpiresAt  int64  `json:"expires_at" binding:"required"`
}

type serviceCaptureRequest struct {
	Amount int64 `json:"amount" binding:"required"`

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

// serviceTokenFromRequest returns the resolved service token the auth
// middleware pinned onto the context, or a (status, message) error pair.
func serviceTokenFromRequest(r *httprequest.Request) (*controlplane.ResolvedServiceToken, int, string) {
	v, ok := r.Get(ginmw.ServiceTokenContextKey)
	if !ok {
		return nil, http.StatusUnauthorized, "service token required"
	}
	resolved, ok := v.(*controlplane.ResolvedServiceToken)
	if !ok || resolved == nil {
		return nil, http.StatusInternalServerError, "service token state invalid"
	}
	return resolved, 0, ""
}

func requireServiceCustomerScope(r *httprequest.Request, tenantSubject billingidentity.CustomerID) bool {
	resolved, status, msg := serviceTokenFromRequest(r)
	if resolved == nil {
		r.ErrorJSON(status, msg)
		return false
	}
	if !resolved.AllowsCustomer(tenantSubject.UUID()) {
		r.ErrorJSON(http.StatusForbidden, "service_token_tenant_subject_scope_denied")
		return false
	}
	return true
}

// serviceAuthorizeRequest is the body of POST /v1/service/credits/authorize
// (issue #235/#247). customer_id is the subject billed; actor is the
// canonical caller for per-(customer_id, actor) sub-budgets;
// estimate_micros = the upper-bound charge;
// request_id = the idempotency key for the placed hold.
type serviceAuthorizeRequest struct {
	CustomerID     string `json:"customer_id"`
	Actor          string `json:"actor"`
	EstimateMicros int64  `json:"estimate_micros"`
	RequestID      string `json:"request_id" binding:"required"`
	ExpiresAt      *int64 `json:"expires_at"`
}

// serviceAuthorizeResponse mirrors the unified authorize contract: the policy
// decision + the tenant subject's real available/outstanding + the placed reservation.
type serviceAuthorizeResponse struct {
	Allowed              bool       `json:"allowed"`
	DenyCode             string     `json:"deny_code,omitempty"`
	BillingMode          string     `json:"billing_mode"`
	AvailableMicros      int64      `json:"available_micros"`
	OutstandingMicros    int64      `json:"outstanding_micros"`
	RemainingTodayMicros *int64     `json:"remaining_today_micros,omitempty"`
	RetryAfterSeconds    int64      `json:"retry_after_seconds,omitempty"`
	ReservationID        *uuid.UUID `json:"reservation_id,omitempty"`
}

// ServiceAuthorizeCredits is the service token-authed policy-decision + ATOMIC hold
// placement (issue #235/#247). The decision (CheckSpendAllowed + prepaid
// available-balance gate) and the hold are run in ONE transaction so two
// concurrent authorizes cannot both pass on the same balance.
//
// TENANT-SUBJECT AUTHORIZATION (issue #246): the acting TENANT is bound by the service token
// (middleware.ServiceTokenRequired pinned it onto the context; RegisterServiceRoutes then
// pinned the RLS connection), NEVER by the body. The tenant subject is read
// from the body but every credit query runs RLS-scoped to the service token's tenant, so a
// tenant subject outside the service token's tenant is structurally unreachable (its rows carry a
// different tenant_id and are invisible — fail closed). The route additionally
// requires PermCreditsSpend (billing:spend), the explicit "may you draw down this
// tenant subject's balance" gate.
func ServiceAuthorizeCredits(r *httprequest.Request) {
	var req serviceAuthorizeRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.EstimateMicros < 0 {
		r.ErrorJSON(http.StatusBadRequest, "estimate_micros must be >= 0")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	tenantSubject, err := parseServiceCustomerID(req.CustomerID)
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

	var expiresAt time.Time
	if req.ExpiresAt != nil {
		expiresAt = time.Unix(*req.ExpiresAt, 0).UTC()
	}

	out, err := svc.AuthorizeAndHold(r.Request.Context(), billingservice.AuthorizeAndHoldRequest{
		CustomerID:     *tenantSubject,
		Actor:          req.Actor,
		EstimateMicros: req.EstimateMicros,
		RequestID:      req.RequestID,
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "authorize failed")
		return
	}

	r.SuccessJSON(serviceAuthorizeResponse{
		Allowed:              out.Allowed,
		DenyCode:             out.DenyCode,
		BillingMode:          out.BillingMode,
		AvailableMicros:      out.AvailableMicros,
		OutstandingMicros:    out.OutstandingOwedMicros,
		RemainingTodayMicros: out.RemainingTodayMicros,
		RetryAfterSeconds:    out.RetryAfterSeconds,
		ReservationID:        out.ReservationID,
	})
}

// serviceBalanceResponse is the tenant-subject balance snapshot served by
// GET /v1/service/credits/balance (issue #235/#247).
type serviceBalanceResponse struct {
	CustomerID            uuid.UUID `json:"customer_id"`
	Currency              string    `json:"currency"`
	BillingMode           string    `json:"billing_mode"`
	BalanceMicros         int64     `json:"balance_micros"`
	HeldMicros            int64     `json:"held_micros"`
	AvailableMicros       int64     `json:"available_micros"`
	OutstandingOwedMicros int64     `json:"outstanding_owed_micros"`
}

// ServiceGetCreditsBalance returns the tenant subject's REAL balance snapshot (issue
// #235/#247): available = balance - held, plus outstanding owed + billing mode.
// Tenant-bound by the service token (RLS); tenant subject supplied via ?customer_id=.
func ServiceGetCreditsBalance(r *httprequest.Request) {
	currency := strings.TrimSpace(r.Request.URL.Query().Get("currency"))
	if currency == "" {
		currency = money.DefaultCurrency
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
		BalanceMicros:         snap.BalanceMicros,
		HeldMicros:            snap.HeldMicros,
		AvailableMicros:       snap.AvailableMicros,
		OutstandingOwedMicros: snap.OutstandingOwedMicros,
	})
}

// serviceQueryCustomer extracts the tenant subject from the query string.
func serviceQueryCustomer(r *httprequest.Request) (*identity.CustomerID, error) {
	return parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
}

// serviceAccountSettingsRequest is the PUT body for configuring a tenant subject's credit
// billing account (issue #242). All settings are optional pointers (only the
// supplied fields are changed); customer_id + currency identify the account.
type serviceAccountSettingsRequest struct {
	CustomerID               string  `json:"customer_id"`
	Currency                 string  `json:"currency"`
	BillingMode              *string `json:"billing_mode"` // "prepaid" | "arrears"
	MaxSpendPerDayMicros     *int64  `json:"max_spend_per_day_micros"`
	MaxSpendPerMonthMicros   *int64  `json:"max_spend_per_month_micros"`
	MaxOutstandingOwedMicros *int64  `json:"max_outstanding_owed_micros"`
	LowBalanceThreshold      *int64  `json:"low_balance_threshold_micros"`
	AutoTopupEnabled         *bool   `json:"auto_topup_enabled"`
	AutoTopupAmountCents     *int64  `json:"auto_topup_amount_cents"`
	AutoTopupPaymentMethod   *string `json:"auto_topup_payment_method_id"`
	DefaultCreditExpiryDays  *int    `json:"default_credit_expiry_days"`
	HardStopOnBreach         *bool   `json:"hard_stop_on_breach"`
	AlertThresholdPct        *int    `json:"alert_threshold_pct"`
}

// ServiceSetCreditAccountSettings (PUT /v1/service/credits/account-settings)
// configures a tenant subject's billing account: prepaid vs arrears mode, spend caps,
// auto-top-up, and expiry default (issue #242). Tensorhub's billing-admin surface
// proxies to this with its service service token; OpenRails owns the settings.
func ServiceSetCreditAccountSettings(r *httprequest.Request) {
	var req serviceAccountSettingsRequest
	if !r.BindJSON(&req) {
		return
	}
	tenantSubject, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubject) {
		return
	}
	currency := strings.TrimSpace(req.Currency)
	if currency == "" {
		currency = money.DefaultCurrency
	}
	in := money.AccountSettingsInput{
		BillingMode:              req.BillingMode,
		MaxSpendPerDayMicros:     req.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros:   req.MaxSpendPerMonthMicros,
		MaxOutstandingOwedMicros: req.MaxOutstandingOwedMicros,
		LowBalanceThreshold:      req.LowBalanceThreshold,
		AutoTopupEnabled:         req.AutoTopupEnabled,
		AutoTopupAmountCents:     req.AutoTopupAmountCents,
		DefaultCreditExpiryDays:  req.DefaultCreditExpiryDays,
		HardStopOnBreach:         req.HardStopOnBreach,
		AlertThresholdPct:        req.AlertThresholdPct,
	}
	if req.AutoTopupPaymentMethod != nil {
		if pm, perr := uuid.Parse(strings.TrimSpace(*req.AutoTopupPaymentMethod)); perr == nil {
			in.AutoTopupPaymentMethod = &pm
		}
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetCreditAccountSettings(r.Request.Context(), *tenantSubject, currency, in); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), *tenantSubject, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(settings)
}

// ServiceGetCreditAccountSettings (GET /v1/service/credits/account-settings)
// reads a tenant subject's current billing-account settings (issue #242).
func ServiceGetCreditAccountSettings(r *httprequest.Request) {
	currency := strings.TrimSpace(r.Request.URL.Query().Get("currency"))
	if currency == "" {
		currency = money.DefaultCurrency
	}
	tenantSubject, err := serviceQueryCustomer(r)
	if err != nil || tenantSubject == nil {
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
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), *tenantSubject, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(settings)
}

// ServiceListCustomerCreditTransactions (GET /v1/service/credits/transactions)
// lists a tenant subject's credit transactions (usage history) for the billing-account
// admin surface (issue #242). Query: customer_id, currency, limit, offset.
func ServiceListCustomerCreditTransactions(r *httprequest.Request) {
	currency := strings.TrimSpace(r.Request.URL.Query().Get("currency"))
	if currency == "" {
		currency = money.DefaultCurrency
	}
	tenantSubject, err := serviceQueryCustomer(r)
	if err != nil || tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *tenantSubject) {
		return
	}
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.GetCustomerCreditTransactions(r.Request.Context(), *tenantSubject, currency, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	out := make([]serviceTxnResponse, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxnResponse{
			ID: t.ID, CustomerID: t.CustomerID, Actor: t.Actor, Amount: t.Amount,
			TransactionType: t.TransactionType, Status: t.Status, Source: t.Source,
			CreatedAt: t.CreatedAt,
		})
	}
	r.SuccessJSON(map[string]any{"transactions": out, "total": total})
}

type serviceTxnResponse struct {
	ID              uuid.UUID `json:"id"`
	CustomerID      uuid.UUID `json:"customer_id"`
	Actor           string    `json:"actor"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

type serviceUsageRollupRequest struct {
	CustomerID string `json:"customer_id" binding:"required"`
	From       int64  `json:"from" binding:"required"` // unix seconds, inclusive
	To         int64  `json:"to" binding:"required"`   // unix seconds, exclusive
	GroupBy    string `json:"group_by" binding:"required"`
}

type serviceEndpointRevenueRequest struct {
	Resource string `json:"resource" binding:"required"`
	From     int64  `json:"from" binding:"required"`
	To       int64  `json:"to" binding:"required"`
}

// ServiceResourceRevenue returns per-day revenue for a resource (by usage_event
// the typed resource column) across all tenant subjects in the tenant (#410) — powers
// tensorhub endpoint revenue analytics. Operator service token, credits:read.
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
	rows, err := svc.ResourceRevenueDaily(r.Request.Context(), req.Resource, time.Unix(req.From, 0).UTC(), time.Unix(req.To, 0).UTC())
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var total int64
	for _, x := range rows {
		total += x.AmountMicros
	}
	r.SuccessJSON(map[string]any{"revenue_micros": total, "daily": rows})
}

// ServiceUsageRollup returns per-dimension-value spend for a tenant subject over a
// window (#311) — the OpenRails-sourced data behind the tensorhub platform's
// /budget-usage + revenue analytics. Operator-service token, credits:read scope.
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
	rows, err := svc.ServiceUsageRollup(r.Request.Context(), billingservice.ServiceUsageRollupRequest{
		CustomerID: tenantSubjectID,
		From:       time.Unix(req.From, 0).UTC(),
		To:         time.Unix(req.To, 0).UTC(),
		GroupBy:    req.GroupBy,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"rows": rows})
}

func ServiceDepositCredits(r *httprequest.Request) {
	var req serviceDepositRequest
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

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}

	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		CustomerID:  tenantSubjectID,
		Actor:       req.Actor,
		Currency:    req.Currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		ExpiresAt:   expiresAt,
		Description: req.Description,
	})
	if err != nil {
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
			return
		}
		// #483: an unknown/invalid currency is a client error (parity with local), not a 500.
		if errors.Is(err, money.ErrBillingUnitRequired) || strings.Contains(err.Error(), "unknown currency") {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "deposit failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceWithdrawCredits(r *httprequest.Request) {
	var req serviceWithdrawRequest
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
	trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
		CustomerID: tenantSubjectID,
		Actor:      req.Actor,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err == billingservice.ErrCreditTypeInactive {
		r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "withdraw failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceHoldCredits(r *httprequest.Request) {
	var req serviceHoldRequest
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
	hold, err := svc.HoldCredits(r.Request.Context(), billingservice.HoldCreditsRequest{
		CustomerID: tenantSubjectID,
		Actor:      req.Actor,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
		ExpiresAt:  time.Unix(req.ExpiresAt, 0).UTC(),
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err == billingservice.ErrCreditTypeInactive {
		r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "hold failed")
		return
	}
	r.SuccessJSON(hold)
}

func ServiceCaptureHold(r *httprequest.Request) {
	holdID, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	var req serviceCaptureRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{
		HoldID:     holdID,
		Amount:     req.Amount,
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
		r.ErrorJSON(http.StatusInternalServerError, "capture failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceReleaseHold(r *httprequest.Request) {
	holdID, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.ReleaseHold(r.Request.Context(), holdID); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "release failed")
		return
	}
	r.SuccessJSON(map[string]any{"ok": true})
}

func ServiceGetActorCredits(r *httprequest.Request) {
	actorID := strings.TrimSpace(r.Param("actor"))
	if actorID == "" {
		r.ErrorJSON(http.StatusBadRequest, "actor required")
		return
	}
	tenantSubjectID, err := parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	// #472: one universal µ$ wallet, no credit_type dimension.
	var balance int64
	var heldBalance int64
	if tenantSubjectID != nil {
		if !requireServiceCustomerScope(r, *tenantSubjectID) {
			return
		}
		bal, err := r.State.MoneyService.GetBalanceForCustomer(r.Request.Context(), *tenantSubjectID, money.DefaultCurrency)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	} else {
		bal, err := r.State.MoneyService.GetBalance(r.Request.Context(), actorID)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	}
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "balance not found")
		return
	}
	r.SuccessJSON(map[string]any{
		"type":         "usd_micro",
		"balance":      balance,
		"held_balance": heldBalance,
	})
}

func ServiceLookupCreditTransaction(r *httprequest.Request) {
	actorID := strings.TrimSpace(r.Request.URL.Query().Get("actor"))
	if actorID == "" {
		r.ErrorJSON(http.StatusBadRequest, "actor required")
		return
	}
	source := strings.TrimSpace(r.Request.URL.Query().Get("source"))
	if source == "" {
		r.ErrorJSON(http.StatusBadRequest, "source required")
		return
	}
	sourceID := strings.TrimSpace(r.Request.URL.Query().Get("source_id"))
	if sourceID == "" {
		r.ErrorJSON(http.StatusBadRequest, "source_id required")
		return
	}
	transactionType := strings.TrimSpace(r.Request.URL.Query().Get("transaction_type"))
	if transactionType == "" {
		transactionType = "hold"
	}

	trx, err := r.State.MoneyService.GetTransactionBySource(r.Request.Context(), actorID, money.DefaultCurrency, transactionType, source, sourceID)
	if err != nil {
		if repo.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "not found")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(trx)
}
