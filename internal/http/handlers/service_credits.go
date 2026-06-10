package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceWithdrawRequest struct {
	TenantSubjectID string     `json:"tenant_subject_id"`
	Actor           string     `json:"actor" binding:"required"`
	CreditType      string     `json:"credit_type" binding:"required"`
	Amount          int64      `json:"amount" binding:"required"`
	Source          string     `json:"source" binding:"required"`
	SourceID        *uuid.UUID `json:"source_id" binding:"required"`
}

type serviceDepositRequest struct {
	TenantSubjectID string     `json:"tenant_subject_id"`
	Actor           string     `json:"actor" binding:"required"`
	CreditType      string     `json:"credit_type" binding:"required"`
	Amount          int64      `json:"amount" binding:"required"`
	Source          string     `json:"source" binding:"required"`
	SourceID        *uuid.UUID `json:"source_id" binding:"required"`
	ExpiresAt       *int64     `json:"expires_at"`
	Description     *string    `json:"description"`
}

type serviceHoldRequest struct {
	TenantSubjectID string `json:"tenant_subject_id"`
	Actor           string `json:"actor" binding:"required"`
	CreditType      string `json:"credit_type" binding:"required"`
	Amount          int64  `json:"amount" binding:"required"`
	Source          string `json:"source" binding:"required"`
	SourceID        string `json:"source_id" binding:"required"`
	ExpiresAt       int64  `json:"expires_at" binding:"required"`
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

func parseServiceTenantSubjectID(raw string) (*billingidentity.TenantSubjectID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return nil, errors.New("invalid tenant_subject_id")
	}
	tenantSubject := billingidentity.TenantSubjectID(id)
	return &tenantSubject, nil
}

func requireServiceTenantSubjectScope(r *httprequest.Request, tenantSubject billingidentity.TenantSubjectID) bool {
	v, ok := r.Get(ginmw.ServiceTokenContextKey)
	if !ok {
		r.ErrorJSON(http.StatusUnauthorized, "service token required")
		return false
	}
	resolved, ok := v.(*controlplane.ResolvedServiceToken)
	if !ok || resolved == nil {
		r.ErrorJSON(http.StatusInternalServerError, "service token state invalid")
		return false
	}
	if !resolved.AllowsTenantSubject(tenantSubject.UUID()) {
		r.ErrorJSON(http.StatusForbidden, "service_token_tenant_subject_scope_denied")
		return false
	}
	return true
}

// serviceAuthorizeRequest is the body of POST /v1/service/credits/authorize
// (issue #235/#247). tenant_subject_id is the subject billed; actor is the
// canonical caller for per-(tenant_subject_id, actor) sub-budgets;
// estimate_micros = the upper-bound charge;
// request_id = the idempotency key for the placed hold.
type serviceAuthorizeRequest struct {
	TenantSubjectID string `json:"tenant_subject_id"`
	Actor           string `json:"actor"`
	CreditType      string `json:"credit_type" binding:"required"`
	EstimateMicros  int64  `json:"estimate_micros"`
	RequestID       string `json:"request_id" binding:"required"`
	ExpiresAt       *int64 `json:"expires_at"`
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

	tenantSubject, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid tenant_subject_id")
		return
	}
	if tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubject) {
		return
	}

	var expiresAt time.Time
	if req.ExpiresAt != nil {
		expiresAt = time.Unix(*req.ExpiresAt, 0).UTC()
	}

	out, err := svc.AuthorizeAndHold(r.Request.Context(), billingservice.AuthorizeAndHoldRequest{
		TenantSubjectID: *tenantSubject,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
		EstimateMicros:  req.EstimateMicros,
		RequestID:       req.RequestID,
		ExpiresAt:       expiresAt,
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
	TenantSubjectID       uuid.UUID `json:"tenant_subject_id"`
	CreditType            string    `json:"credit_type"`
	BillingMode           string    `json:"billing_mode"`
	BalanceMicros         int64     `json:"balance_micros"`
	HeldMicros            int64     `json:"held_micros"`
	AvailableMicros       int64     `json:"available_micros"`
	OutstandingOwedMicros int64     `json:"outstanding_owed_micros"`
}

// ServiceGetCreditsBalance returns the tenant subject's REAL balance snapshot (issue
// #235/#247): available = balance - held, plus outstanding owed + billing mode.
// Tenant-bound by the service token (RLS); tenant subject supplied via ?tenant_subject_id=.
func ServiceGetCreditsBalance(r *httprequest.Request) {
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		creditType = strings.TrimSpace(r.Request.URL.Query().Get("type"))
	}
	if creditType == "" {
		creditType = "api_credits"
	}
	tenantSubject, err := parseServiceTenantSubjectID(r.Request.URL.Query().Get("tenant_subject_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid tenant_subject_id")
		return
	}
	if tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubject) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	snap, err := svc.GetCreditAccount(r.Request.Context(), *tenantSubject, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(serviceBalanceResponse{
		TenantSubjectID:       snap.TenantSubjectID,
		CreditType:            snap.CreditType,
		BillingMode:           snap.BillingMode,
		BalanceMicros:         snap.BalanceMicros,
		HeldMicros:            snap.HeldMicros,
		AvailableMicros:       snap.AvailableMicros,
		OutstandingOwedMicros: snap.OutstandingOwedMicros,
	})
}

// serviceQueryTenantSubject extracts the tenant subject from the query string.
func serviceQueryTenantSubject(r *httprequest.Request) (*identity.TenantSubjectID, error) {
	return parseServiceTenantSubjectID(r.Request.URL.Query().Get("tenant_subject_id"))
}

// serviceAccountSettingsRequest is the PUT body for configuring a tenant subject's credit
// billing account (issue #242). All settings are optional pointers (only the
// supplied fields are changed); tenant_subject_id + credit_type identify the account.
type serviceAccountSettingsRequest struct {
	TenantSubjectID          string  `json:"tenant_subject_id"`
	CreditType               string  `json:"credit_type"`
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
	tenantSubject, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil || tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubject) {
		return
	}
	creditType := strings.TrimSpace(req.CreditType)
	if creditType == "" {
		creditType = "api_credits"
	}
	in := credits.AccountSettingsInput{
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
	if err := svc.SetCreditAccountSettings(r.Request.Context(), *tenantSubject, creditType, in); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), *tenantSubject, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(settings)
}

// ServiceGetCreditAccountSettings (GET /v1/service/credits/account-settings)
// reads a tenant subject's current billing-account settings (issue #242).
func ServiceGetCreditAccountSettings(r *httprequest.Request) {
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		creditType = "api_credits"
	}
	tenantSubject, err := serviceQueryTenantSubject(r)
	if err != nil || tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubject) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), *tenantSubject, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(settings)
}

// ServiceListTenantSubjectCreditTransactions (GET /v1/service/credits/transactions)
// lists a tenant subject's credit transactions (usage history) for the billing-account
// admin surface (issue #242). Query: tenant_subject_id, credit_type, limit, offset.
func ServiceListTenantSubjectCreditTransactions(r *httprequest.Request) {
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		creditType = "api_credits"
	}
	tenantSubject, err := serviceQueryTenantSubject(r)
	if err != nil || tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubject) {
		return
	}
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.GetTenantSubjectCreditTransactions(r.Request.Context(), *tenantSubject, creditType, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	out := make([]serviceTxnResponse, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxnResponse{
			ID: t.ID, TenantSubjectID: t.TenantSubjectID, Actor: t.Actor, Amount: t.Amount,
			TransactionType: t.TransactionType, Status: t.Status, Source: t.Source,
			CreatedAt: t.CreatedAt,
		})
	}
	r.SuccessJSON(map[string]any{"transactions": out, "total": total})
}

type serviceTxnResponse struct {
	ID              uuid.UUID `json:"id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Actor           string    `json:"actor"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

type serviceUsageRollupRequest struct {
	TenantSubjectID string `json:"tenant_subject_id" binding:"required"`
	From            int64  `json:"from" binding:"required"` // unix seconds, inclusive
	To              int64  `json:"to" binding:"required"`   // unix seconds, exclusive
	GroupBy         string `json:"group_by" binding:"required"`
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
		total += x.AmountMillicents
	}
	r.SuccessJSON(map[string]any{"revenue_millicents": total, "daily": rows})
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
	tenantSubjectID, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubjectID) {
		return
	}
	rows, err := svc.ServiceUsageRollup(r.Request.Context(), billingservice.ServiceUsageRollupRequest{
		TenantSubjectID: tenantSubjectID,
		From:            time.Unix(req.From, 0).UTC(),
		To:              time.Unix(req.To, 0).UTC(),
		GroupBy:         req.GroupBy,
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

	tenantSubjectID, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubjectID) {
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}

	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		TenantSubjectID: tenantSubjectID,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
		ExpiresAt:       expiresAt,
		Description:     req.Description,
	})
	if err != nil {
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
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
	tenantSubjectID, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubjectID) {
		return
	}
	trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
		TenantSubjectID: tenantSubjectID,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
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
	tenantSubjectID, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubjectID == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *tenantSubjectID) {
		return
	}
	hold, err := svc.HoldCredits(r.Request.Context(), billingservice.HoldCreditsRequest{
		TenantSubjectID: tenantSubjectID,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
		ExpiresAt:       time.Unix(req.ExpiresAt, 0).UTC(),
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
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("type"))
	if creditType == "" {
		creditType = "api_credits"
	}
	tenantSubjectID, err := parseServiceTenantSubjectID(r.Request.URL.Query().Get("tenant_subject_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var balance int64
	var heldBalance int64
	if tenantSubjectID != nil {
		if !requireServiceTenantSubjectScope(r, *tenantSubjectID) {
			return
		}
		bal, err := r.State.CreditsService.GetBalanceForTenantSubject(r.Request.Context(), *tenantSubjectID, creditType)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	} else {
		bal, err := r.State.CreditsService.GetBalance(r.Request.Context(), actorID, creditType)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	}
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}
	r.SuccessJSON(map[string]any{
		"type":         creditType,
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
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		r.ErrorJSON(http.StatusBadRequest, "credit_type required")
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

	trx, err := r.State.CreditsService.GetTransactionBySource(r.Request.Context(), actorID, creditType, transactionType, source, sourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "not found")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(trx)
}
