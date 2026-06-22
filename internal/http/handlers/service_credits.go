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
	Invoker    string     `json:"invoker"`
	Currency   string     `json:"currency"`
	Amount     int64      `json:"amount" binding:"required"`
	Source     string     `json:"source" binding:"required"`
	SourceID   *uuid.UUID `json:"source_id" binding:"required"`
}

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
	v, ok := r.Get(ginmw.ServiceCredentialContextKey)
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
	if v, ok := r.Get(ginmw.ServiceCredentialContextKey); ok {
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
	if v, ok := r.Get(ginmw.ServiceCredentialContextKey); ok {
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
// GET /v1/service/credits/balance (issue #235/#247).
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

// serviceQueryCustomer extracts the customer from the query string.
func serviceQueryCustomer(r *httprequest.Request) (*identity.CustomerID, error) {
	return parseServiceCustomerID(r.Request.URL.Query().Get("customer_id"))
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

// serviceAccountSettingsRequest is the PUT body for configuring a customer's credit
// billing account (issue #242). All settings are optional pointers (only the
// supplied fields are changed); customer_id + currency identify the account.
type serviceAccountSettingsRequest struct {
	CustomerID               string  `json:"customer_id"`
	Currency                 string  `json:"currency"`
	BillingMode              *string `json:"billing_mode"` // "prepaid" | "arrears"
	MaxSpendPerDay           *int64  `json:"max_spend_per_day"`
	MaxSpendPerMonth         *int64  `json:"max_spend_per_month"`
	MaxOutstandingOwedAmount *int64  `json:"max_outstanding_owed_amount"`
	LowBalanceThreshold      *int64  `json:"low_balance_threshold"`
	AutoTopupEnabled         *bool   `json:"auto_topup_enabled"`
	AutoTopupAmount          *int64  `json:"auto_topup_amount"`
	AutoTopupPaymentMethod   *string `json:"auto_topup_payment_method_id"`
	DefaultCreditExpiryDays  *int    `json:"default_credit_expiry_days"`
	HardStopOnBreach         *bool   `json:"hard_stop_on_breach"`
	AlertThresholdPct        *int    `json:"alert_threshold_pct"`
}

// ServiceSetCreditAccountSettings (PUT /v1/service/credits/account-settings)
// configures a customer's billing account: prepaid vs arrears mode, spend caps,
// auto-top-up, and expiry default (issue #242). Tensorhub's billing-admin surface
// proxies to this with its service API key; OpenRails owns the settings.
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
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	in := money.AccountSettingsInput{
		BillingMode:              req.BillingMode,
		MaxSpendPerDay:           req.MaxSpendPerDay,
		MaxSpendPerMonth:         req.MaxSpendPerMonth,
		MaxOutstandingOwedAmount: req.MaxOutstandingOwedAmount,
		LowBalanceThreshold:      req.LowBalanceThreshold,
		AutoTopupEnabled:         req.AutoTopupEnabled,
		AutoTopupAmountCents:     req.AutoTopupAmount,
		DefaultCreditExpiryDays:  req.DefaultCreditExpiryDays,
		HardStopOnBreach:         req.HardStopOnBreach,
		AlertThresholdPct:        req.AlertThresholdPct,
	}
	if req.AutoTopupPaymentMethod != nil {
		pm, perr := uuid.Parse(strings.TrimSpace(*req.AutoTopupPaymentMethod))
		if perr != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid auto_topup_payment_method_id")
			return
		}
		if r.State.PaymentMethodService == nil {
			r.ErrorJSON(http.StatusInternalServerError, "payment method service unavailable")
			return
		}
		if err := r.State.PaymentMethodService.ValidateOwnership(r.Request.Context(), pm, tenantSubject.String()); err != nil {
			r.ErrorJSON(http.StatusBadRequest, "auto_topup_payment_method_id does not belong to this customer")
			return
		}
		in.AutoTopupPaymentMethod = &pm
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
// reads a customer's current billing-account settings (issue #242).
func ServiceGetCreditAccountSettings(r *httprequest.Request) {
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
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
// lists a customer's credit transactions (usage history) for the billing-account
// admin surface (issue #242). Query: customer_id, currency, limit, offset.
func ServiceListCustomerCreditTransactions(r *httprequest.Request) {
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
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
			ID: t.ID, CustomerID: t.CustomerID, Invoker: t.Invoker, Amount: t.Amount,
			Currency: t.Currency, TransactionType: t.TransactionType, Status: t.Status, Source: t.Source,
			CreatedAt: t.CreatedAt,
		})
	}
	r.SuccessJSON(map[string]any{"transactions": out, "total": total})
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

func ServiceWithdrawCredits(r *httprequest.Request) {
	var req serviceWithdrawRequest
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
	trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
		CustomerID: tenantSubjectID,
		Invoker:    invoker,
		Currency:   currency,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "withdraw failed")
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
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{
		RequestID:  requestID,
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

func ServiceLookupCreditTransaction(r *httprequest.Request) {
	invokerID := strings.TrimSpace(r.Request.URL.Query().Get("invoker"))
	if invokerID == "" {
		r.ErrorJSON(http.StatusBadRequest, "invoker required")
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
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	trx, err := r.State.MoneyService.GetTransactionBySource(r.Request.Context(), invokerID, currency, transactionType, source, sourceID)
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
