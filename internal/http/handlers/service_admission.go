package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/money"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// maxAdmitBatchItems bounds one /v1/merchant/admissions request (#335).
const maxAdmitBatchItems = 1000

type serviceAdmitRequest struct {
	CustomerID string `json:"customer_id"`
	// Invoker is the end-user attribution/abuse label (#491).
	Invoker         string `json:"invoker"`
	InvokerType     string `json:"invoker_type"`
	TrustLevel      string `json:"trust_level"`
	Resource        string `json:"resource"`
	Currency        string `json:"currency"`
	EstimatedAmount int64  `json:"estimated_amount"`
	RequestID       string `json:"request_id"`
	Source          string `json:"source"`
	ExpiresAt       *int64 `json:"expires_at"`
	// Roles are the invoker's immutable role UUIDs (#473) — each (subject, role)
	// budget-scope policy gates this request's spend.
	Roles []uuid.UUID `json:"roles"`
}

// admitInputFromRequest maps one admission item onto the service-facade input.
func admitInputFromRequest(req serviceAdmitRequest, payer billingidentity.CustomerID) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		TrustLevel:      strings.TrimSpace(req.TrustLevel),
		Resource:        req.Resource,
		Currency:        req.Currency,
		EstimatedAmount: req.EstimatedAmount,
		Source:          req.Source,
		SourceID:        req.RequestID,
		Roles:           req.Roles,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}
	return in
}

// admitVerdictStatus maps an admission result onto the per-item HTTP-equivalent
// status returned by the batch route.
func admitVerdictStatus(res *billingservice.AdmitResult) int {
	if res.Allowed {
		return http.StatusOK
	}
	switch res.BlockedBy {
	case "abuse":
		return http.StatusTooManyRequests
	case "money":
		return http.StatusPaymentRequired
	default:
		return http.StatusForbidden
	}
}

type serviceAdmitBatchRequest struct {
	Items []serviceAdmitRequest `json:"items"`
}

// serviceAdmitVerdict is one per-item batch outcome (#335). Status is the
// HTTP-equivalent status for this item; Result carries the full admission
// decision when one was reached.
type serviceAdmitVerdict struct {
	Status int                         `json:"status"`
	Error  string                      `json:"error,omitempty"`
	Result *billingservice.AdmitResult `json:"result,omitempty"`
}

// serviceAdmitBatchVerdicts runs the per-item admission loop with FULL per-item
// isolation: a bad payer id, scope denial, deny, or backend error on one item
// never fails the others. allows gates each item's payer against the service
// API key's customer scope; admit is the (injected) single-admit core.
//
// Follow-up (#335): each item currently runs the full single-admit path. An
// obvious optimization is batching shared payer reads inside one transaction.
func serviceAdmitBatchVerdicts(
	ctx context.Context,
	items []serviceAdmitRequest,
	allows func(billingidentity.CustomerID) bool,
	admit func(context.Context, billingservice.AdmitInput) (*billingservice.AdmitResult, error),
) []serviceAdmitVerdict {
	out := make([]serviceAdmitVerdict, len(items))
	for i, item := range items {
		if item.EstimatedAmount < 0 {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "estimated_amount must be >= 0"}
			continue
		}
		payer, err := parseServiceCustomerID(item.CustomerID)
		if err != nil || payer == nil {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "customer_id required"}
			continue
		}
		if !allows(*payer) {
			out[i] = serviceAdmitVerdict{Status: http.StatusForbidden, Error: "service_credential_customer_scope_denied"}
			continue
		}
		res, err := admit(ctx, admitInputFromRequest(item, *payer))
		if err != nil {
			out[i] = serviceAdmitVerdict{Status: http.StatusInternalServerError, Error: "admission check failed"}
			continue
		}
		out[i] = serviceAdmitVerdict{Status: admitVerdictStatus(res), Result: res}
	}
	return out
}

// ServiceAdmitBatch is the cross-payer batch admission endpoint (#335): one
// request carries N admit items (mixed payers); the response carries N
// positional verdicts with the same semantics as a single admission per item.
// The batch itself always answers 200 — per-item denial/errors live in the
// items, so cold payers conflating admits collapse N hops into one without one
// broke payer poisoning the flight.
func ServiceAdmitBatch(r *httprequest.Request) {
	var req serviceAdmitBatchRequest
	if !r.BindJSON(&req) {
		return
	}
	if len(req.Items) == 0 {
		r.ErrorJSON(http.StatusBadRequest, "items required")
		return
	}
	if len(req.Items) > maxAdmitBatchItems {
		r.ErrorJSON(http.StatusBadRequest, "too many items")
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	verdicts := serviceAdmitBatchVerdicts(
		r.Request.Context(),
		req.Items,
		func(ts billingidentity.CustomerID) bool { return serviceCustomerScopeAllows(r, ts) },
		svc.Admit,
	)
	r.JSON(http.StatusOK, map[string]any{"items": verdicts})
}

// ServiceGetTrustLevel returns the payer's current trust level (#477): the value
// OpenRails auto-maintains from cumulative paid spend against the persisted
// trust-level schedule (#476). customer_id is a query param; the merchant is
// pinned from the API key. Empty trust_level means the payer has never graduated
// (caller treats it as the lowest/default). Operator API key, credits:read.
func ServiceGetTrustLevel(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Query("currency"))
	if !ok {
		return
	}
	if err := money.ValidateCurrency(currency); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trustLevel, err := svc.GetTrustLevel(r.Request.Context(), *payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "trust level lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "trust_level": trustLevel})
}

type serviceReportWastedSpendRequest struct {
	CustomerID  string `json:"customer_id"`
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type"`
	Currency    string `json:"currency"`
	Amount      int64  `json:"amount"`
	Source      string `json:"source"`
	SourceID    string `json:"source_id"`
	Reason      string `json:"reason"`
}

// ServiceReportWastedSpend records host-reported WASTED $ (#497): delegated
// invokers accrue toward their flat cutoff; direct payer credentials use
// trust-level grace and then normal ledger charging. Operator API key,
// credits:write.
func ServiceReportWastedSpend(r *httprequest.Request) {
	var req serviceReportWastedSpendRequest
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
	if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.SourceID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "source and source_id required")
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
	res, err := svc.ReportWastedSpend(r.Request.Context(), billingservice.WastedSpendInput{
		CustomerID:  *payer,
		Invoker:     strings.TrimSpace(req.Invoker),
		InvokerType: req.InvokerType,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		Reason:      req.Reason,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "report wasted spend failed")
		return
	}
	r.JSON(http.StatusOK, res)
}

type serviceCreditLimitRequest struct {
	CustomerID        string `json:"customer_id"`
	Currency          string `json:"currency"`
	CreditLimitAmount int64  `json:"credit_limit_amount"`
}

// ServiceSetCreditLimit sets the admin/operator arrears credit line for a payer
// (#489): under billing_mode=arrears the balance may go NEGATIVE up to the limit;
// AdmitHold denies insufficient_credit when a new hold would exceed it. 0 = off.
// Merchant-admin gated at the route (`merchant:customer-settings:update`) - NOT self-serve.
func ServiceSetCreditLimit(r *httprequest.Request) {
	var req serviceCreditLimitRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.CreditLimitAmount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "credit_limit_amount must be >= 0")
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
	if err := svc.SetCreditLimit(r.Request.Context(), *payer, currency, req.CreditLimitAmount); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceGetCreditLimit returns the admin-set arrears credit line for a payer
// (#489). customer_id is a query param; the merchant is pinned from the API key.
// Operator API key, credits:read.
func ServiceGetCreditLimit(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Query("currency"))
	if !ok {
		return
	}
	v, err := svc.GetCreditLimit(r.Request.Context(), *payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "credit_limit_amount": v})
}

// serviceMerchantConfigWindow is one amount window in the merchant configuration
// request body.
type serviceMerchantConfigWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

type serviceMerchantSettingsRequest struct {
	Profile                           *serviceMerchantProfileConfiguration  `json:"profile,omitempty"`
	InvoiceCollectionThreshold        *int64                                `json:"collection_threshold,omitempty"`
	InvoiceMonthlyFloor               *int64                                `json:"monthly_floor,omitempty"`
	InvoiceBillingBoundary            string                                `json:"billing_period_boundary,omitempty"`
	TrustLevelSchedules               []serviceMerchantTrustLevelSchedule   `json:"trust_level_schedules,omitempty"`
	TrustLevelSpendLimits             []billingservice.PayerSpendLimitInput `json:"trust_level_spend_limits,omitempty"`
	DelegatedInvokerWastedSpendLimits []serviceMerchantConfigWindow         `json:"delegated_invoker_wasted_spend_limits,omitempty"`
}

type serviceMerchantTrustLevelSchedule struct {
	Currency string                                  `json:"currency"`
	Schedule []billingservice.TrustLevelScheduleRung `json:"schedule"`
}

type serviceMerchantProfileConfiguration struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
}

// ServiceGetMerchantSettings returns the merchant-owned policy-sync document.
func ServiceGetMerchantSettings(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	cfg, _, err := svc.GetMerchantConfiguration(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "get merchant settings failed")
		return
	}
	r.SuccessJSON(serviceMerchantSettingsRequest{
		Profile:                           merchantProfileResponsePtr(cfg.Profile),
		InvoiceCollectionThreshold:        cfg.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor:               cfg.InvoiceMonthlyFloor,
		InvoiceBillingBoundary:            cfg.InvoiceBillingBoundary,
		DelegatedInvokerWastedSpendLimits: serviceMerchantConfigWindows(cfg.DelegatedInvokerWastedSpendWindows),
	})
}

// ServiceSetMerchantSettings installs the merchant-owned admission policy in
// one document: profile, trust-level schedules, trust-level spend policies, and
// delegated-invoker wasted-spend limits.
func ServiceSetMerchantSettings(r *httprequest.Request) {
	var req serviceMerchantSettingsRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	windows := make([]abuse.WastedWindow, 0, len(req.DelegatedInvokerWastedSpendLimits))
	for _, w := range req.DelegatedInvokerWastedSpendLimits {
		windows = append(windows, abuse.WastedWindow{
			Key:      w.Key,
			Window:   time.Duration(w.WindowSeconds) * time.Second,
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	if err := svc.SetMerchantConfiguration(r.Request.Context(), billingservice.MerchantConfiguration{
		Profile:                            merchantProfileInput(req.Profile),
		InvoiceCollectionThreshold:         req.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor:                req.InvoiceMonthlyFloor,
		InvoiceBillingBoundary:             req.InvoiceBillingBoundary,
		DelegatedInvokerWastedSpendWindows: windows,
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set merchant settings failed")
		return
	}

	for _, schedule := range req.TrustLevelSchedules {
		currency, ok := serviceRequiredCurrency(r, schedule.Currency)
		if !ok {
			return
		}
		if err := money.ValidateCurrency(currency); err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if err := svc.SetTrustLevelSchedule(r.Request.Context(), billingidentity.CustomerID{}, currency, schedule.Schedule); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "set trust level schedule failed")
			return
		}
	}
	for _, policy := range req.TrustLevelSpendLimits {
		if err := svc.SetPayerSpendLimits(r.Request.Context(), billingidentity.CustomerID{}, policy); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "set trust-level policy failed")
			return
		}
	}
	r.SuccessJSONMessage("ok")
}

func merchantProfileInput(in *serviceMerchantProfileConfiguration) *models.MerchantProfileConfiguration {
	if in == nil {
		return nil
	}
	return &models.MerchantProfileConfiguration{
		DisplayName: strings.TrimSpace(in.DisplayName),
		LogoURL:     strings.TrimSpace(in.LogoURL),
		FromEmail:   strings.TrimSpace(in.FromEmail),
		SupportURL:  strings.TrimSpace(in.SupportURL),
	}
}

func merchantProfileResponsePtr(in *models.MerchantProfileConfiguration) *serviceMerchantProfileConfiguration {
	if in == nil {
		return nil
	}
	out := merchantProfileResponse(in)
	return &out
}

func merchantProfileResponse(in *models.MerchantProfileConfiguration) serviceMerchantProfileConfiguration {
	if in == nil {
		return serviceMerchantProfileConfiguration{}
	}
	return serviceMerchantProfileConfiguration{
		DisplayName: in.DisplayName,
		LogoURL:     in.LogoURL,
		FromEmail:   in.FromEmail,
		SupportURL:  in.SupportURL,
	}
}

func serviceMerchantConfigWindows(in []abuse.WastedWindow) []serviceMerchantConfigWindow {
	out := make([]serviceMerchantConfigWindow, 0, len(in))
	for _, w := range in {
		out = append(out, serviceMerchantConfigWindow{
			Key:           w.Key,
			WindowSeconds: int64(w.Window / time.Second),
			Limit:         w.Limit,
			Currency:      w.Currency,
		})
	}
	return out
}
