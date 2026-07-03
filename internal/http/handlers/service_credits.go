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

type serviceDepositRequest struct {
	CustomerID  string     `json:"customer_id"`
	Invoker     string     `json:"invoker"`
	Currency    string     `json:"currency"`
	Amount      int64      `json:"amount" binding:"required"`
	Source      string     `json:"source" binding:"required"`
	SourceID    *uuid.UUID `json:"source_id" binding:"required"`
	ExpiresAt   *int64     `json:"expires_at"`
	Description *string    `json:"description"`
}

type serviceCaptureRequest struct {
	Amount int64 `json:"amount" binding:"required"`

	// Fallback payer coordinates (#676, additive): used only when the admit-time
	// Redis pointer is gone, so a rendered service stays chargeable. admit_source
	// must echo the admit's source (defaults to "admit").
	CustomerID  string `json:"customer_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Invoker     string `json:"invoker,omitempty"`
	AdmitSource string `json:"admit_source,omitempty"`

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

	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		CustomerID:  tenantSubjectID,
		Invoker:     invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		ExpiresAt:   expiresAt,
		Description: req.Description,
	})
	if err != nil {
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
		RequestID:   requestID,
		Amount:      req.Amount,
		CustomerID:  req.CustomerID,
		Currency:    req.Currency,
		Invoker:     req.Invoker,
		AdmitSource: req.AdmitSource,
		EventType:   req.EventType,
		Resource:    req.Resource,
		Dimensions:  req.Dimensions,
		Metadata:    req.Metadata,
		Source:      req.Source,
		SourceID:    req.SourceID,
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

func ServiceGetInvokerCredits(r *httprequest.Request) {
	invokerID := strings.TrimSpace(r.Param("invoker"))
	if invokerID == "" {
		r.ErrorJSON(http.StatusBadRequest, "invoker required")
		return
	}
	tenantSubjectID, err := parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}
	var balance int64
	var heldBalance int64
	var balanceErr error
	if tenantSubjectID != nil {
		if !requireServiceCustomerScope(r, *tenantSubjectID) {
			return
		}
		bal, err := r.State.MoneyService.GetBalanceForCustomer(r.Request.Context(), *tenantSubjectID, currency)
		if err != nil {
			balanceErr = err
		} else {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	} else {
		bal, err := r.State.MoneyService.GetBalance(r.Request.Context(), invokerID, currency)
		if err != nil {
			balanceErr = err
		} else {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	}
	if balanceErr != nil {
		r.ErrorJSON(http.StatusNotFound, "balance not found")
		return
	}
	r.SuccessJSON(map[string]any{
		"currency":     currency,
		"balance":      balance,
		"held_balance": heldBalance,
	})
}
