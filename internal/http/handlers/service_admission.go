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

// maxAdmitBatchItems bounds one /v1/service/admit-batch request (#335).
const maxAdmitBatchItems = 1000

type serviceAdmitRequest struct {
	CustomerID string `json:"customer_id"`
	// Invoker is the end-user attribution/abuse label (#491).
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type"`
	TrustTier   string `json:"trust_tier"`
	// Tier is a deprecated alias for TrustTier, kept while current clients migrate.
	Tier            string `json:"tier"`
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

// ServiceAdmit checks delegated spend, wasted-spend, and money capacity in one
// decision. Endpoint/resource authorization stays with the host before billing
// admission.
func ServiceAdmit(r *httprequest.Request) {
	var req serviceAdmitRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.EstimatedAmount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "estimated_amount must be >= 0")
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

	in := admitInputFromRequest(req, *payer)
	in.Currency = currency

	res, err := svc.Admit(r.Request.Context(), in)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "admission check failed")
		return
	}

	if res.Allowed {
		r.JSON(http.StatusOK, res)
		return
	}
	switch res.BlockedBy {
	case "abuse":
		r.JSON(http.StatusTooManyRequests, res)
	case "money":
		r.JSON(http.StatusPaymentRequired, res)
	default:
		r.JSON(http.StatusForbidden, res)
	}
}

// admitInputFromRequest maps one admit body onto the service-facade input —
// shared by the single /admit route and each /admit/batch item.
func admitInputFromRequest(req serviceAdmitRequest, payer billingidentity.CustomerID) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		Tier:            trustTier(req.TrustTier, req.Tier),
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

func trustTier(primary, alias string) string {
	if v := strings.TrimSpace(primary); v != "" {
		return v
	}
	return strings.TrimSpace(alias)
}

// admitVerdictStatus maps an admission result onto the HTTP status the single
// /admit route would return — reused per-item by the batch route.
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
// HTTP-equivalent status the single /admit route would have returned for this
// item; Result carries the full admission decision when one was reached.
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
// positional verdicts with the same semantics as /v1/service/admit per item.
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
	resolved, status, msg := serviceCredentialFromRequest(r)
	if resolved == nil {
		r.ErrorJSON(status, msg)
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
		func(ts billingidentity.CustomerID) bool { return resolved.AllowsCustomer(ts.UUID()) },
		svc.Admit,
	)
	r.JSON(http.StatusOK, map[string]any{"items": verdicts})
}

// ServiceGetBudget returns the invoker's fixed money-budget windows (#304
// introspection) for a host's /status dashboard. customer_id + invoker +
// trust_tier are query params; the merchant is pinned from the API key.
func ServiceGetBudget(r *httprequest.Request) {
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
	invoker := r.Query("invoker")
	currency, ok := serviceRequiredCurrency(r, r.Query("currency"))
	if !ok {
		return
	}
	windows, err := svc.BudgetStatus(r.Request.Context(), *payer, invoker, currency, trustTierQuery(r))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "budget lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "windows": windows})
}

// ServiceGetTier returns the payer's current trust tier (#477): the value
// OpenRails auto-maintains from cumulative paid spend against the persisted
// trust-tier schedule (#476). customer_id is a query param; the merchant is
// pinned from the API key. Empty trust_tier means the payer has never graduated
// (caller treats it as the lowest/default). Operator API key, credits:read.
func ServiceGetTier(r *httprequest.Request) {
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
	tier, err := svc.GetTier(r.Request.Context(), *payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "tier lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "trust_tier": tier})
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
// trust-tier grace and then normal ledger charging. Operator API key,
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

// ServiceAbuseUsage returns the payer's + invoker's running WASTED-$ totals +
// budgets (#488 introspection). customer_id + optional invoker + currency +
// trust_tier are query params; the merchant is pinned from the API key. Operator
// API key, credits:read.
func ServiceAbuseUsage(r *httprequest.Request) {
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
	pw, aw, err := svc.AbuseUsage(r.Request.Context(), *payer, r.Query("invoker"), currency, trustTierQuery(r))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "abuse usage lookup failed")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"currency": currency, "payer_windows": pw, "invoker_windows": aw})
}

type serviceCreditLimitRequest struct {
	CustomerID        string `json:"customer_id"`
	Currency          string `json:"currency"`
	CreditLimitAmount int64  `json:"credit_limit_amount"`
}

func trustTierQuery(r *httprequest.Request) string {
	return trustTier(r.Query("trust_tier"), r.Query("tier"))
}

// ServiceSetCreditLimit sets the admin/operator arrears credit line for a payer
// (#489): under billing_mode=arrears the balance may go NEGATIVE up to the limit;
// AdmitHold denies insufficient_credit when a new hold would exceed it. 0 = off.
// Merchant-admin gated at the route (`org:credits:update`) - NOT self-serve.
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

type servicePayerSpendLimitRequest struct {
	CustomerID string `json:"customer_id"`
	billingservice.PayerSpendLimitInput
}

// ServiceSetPayerSpendLimits upserts a trust-tier money policy (#298 tier admin API). An
// EMPTY customer_id sets the merchant-wide DEFAULT policy for the trust tier (#477,
// the platform capacity ladder declared once); a non-empty one sets a
// per-subject override (scope-checked).
func ServiceSetPayerSpendLimits(r *httprequest.Request) {
	var req servicePayerSpendLimitRequest
	if !r.BindJSON(&req) {
		return
	}
	var payer billingidentity.CustomerID
	if strings.TrimSpace(req.CustomerID) != "" {
		p, err := parseServiceCustomerID(req.CustomerID)
		if err != nil || p == nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
			return
		}
		if !requireServiceCustomerScope(r, *p) {
			return
		}
		payer = *p
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetPayerSpendLimits(r.Request.Context(), payer, req.PayerSpendLimitInput); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set trust-tier policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

type serviceTierScheduleRequest struct {
	CustomerID string                            `json:"customer_id"`
	Currency   string                            `json:"currency"`
	Schedule   []billingservice.TierScheduleRung `json:"schedule"`
}

// ServiceSetTierSchedule persists the merchant's tier SCHEDULE (#476): the host
// declares the ladder ONCE and OpenRails AUTO-maintains each payer's trust tier from
// cumulative spend. An EMPTY customer_id sets the merchant-wide default
// schedule (the common case); a non-empty one sets a per-subject override
// (scope-checked). owner=platform. Operator API key, credits:write.
func ServiceSetTierSchedule(r *httprequest.Request) {
	var req serviceTierScheduleRequest
	if !r.BindJSON(&req) {
		return
	}
	var payer billingidentity.CustomerID
	if strings.TrimSpace(req.CustomerID) != "" {
		p, err := parseServiceCustomerID(req.CustomerID)
		if err != nil || p == nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
			return
		}
		if !requireServiceCustomerScope(r, *p) {
			return
		}
		payer = *p
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
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
	if err := svc.SetTierSchedule(r.Request.Context(), payer, currency, req.Schedule); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set tier schedule failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// serviceMerchantConfigWindow is one amount window in the merchant configuration
// request body.
type serviceMerchantConfigWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

type serviceMerchantConfigurationRequest struct {
	Profile                            *serviceMerchantProfileConfiguration `json:"profile,omitempty"`
	DelegatedInvokerWastedSpendWindows []serviceMerchantConfigWindow        `json:"delegated_invoker_wasted_spend_windows"`
}

type serviceMerchantProfileConfiguration struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
}

func ServiceGetMerchantConfiguration(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	cfg, found, err := svc.GetMerchantConfiguration(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "get merchant configuration failed")
		return
	}
	r.SuccessJSON(map[string]any{
		"found":                                  found,
		"profile":                                merchantProfileResponse(cfg.Profile),
		"delegated_invoker_wasted_spend_windows": serviceMerchantConfigWindows(cfg.DelegatedInvokerWastedSpendWindows),
	})
}

// ServiceSetMerchantConfiguration persists the merchant-scoped configuration
// row. Operator API key or delegated merchant admin with configuration:write.
func ServiceSetMerchantConfiguration(r *httprequest.Request) {
	var req serviceMerchantConfigurationRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	windows := make([]abuse.WastedWindow, 0, len(req.DelegatedInvokerWastedSpendWindows))
	for _, w := range req.DelegatedInvokerWastedSpendWindows {
		windows = append(windows, abuse.WastedWindow{
			Key:      w.Key,
			Window:   time.Duration(w.WindowSeconds) * time.Second,
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	if err := svc.SetMerchantConfiguration(r.Request.Context(), billingservice.MerchantConfiguration{
		Profile:                            merchantProfileInput(req.Profile),
		DelegatedInvokerWastedSpendWindows: windows,
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set merchant configuration failed")
		return
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

// spendLimitWindow is one fixed money-budget window in a budget-scope policy
// request/response (#473) — same shape as billingservice.SpendLimitWindowInput.
type spendLimitWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

func spendLimitWindowInputs(ws []spendLimitWindow) []billingservice.SpendLimitWindowInput {
	out := make([]billingservice.SpendLimitWindowInput, 0, len(ws))
	for _, w := range ws {
		out = append(out, billingservice.SpendLimitWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out
}

type serviceInvokerSpendLimitRequest struct {
	CustomerID string             `json:"customer_id"`
	Scope      string             `json:"scope"`
	ScopeKey   string             `json:"scope_key"`
	RoleID     string             `json:"role_id"`
	Windows    []spendLimitWindow `json:"windows"`
}

// ServiceSetInvokerSpendLimits upserts a SUBJECT-owned hierarchical
// budget-scope policy (#473): a self cap (scope=subject), a role pool
// (scope=role; scope_key/role_id is the role uuid), an invoker grant
// (scope=invoker; scope_key is the invoker string), or an invoker-tier grant
// (scope=invoker_tier; scope_key is the tier key). The owner is forced to
// "subject" by the service facade. Operator API key, credits:write.
func ServiceSetInvokerSpendLimits(r *httprequest.Request) {
	var req serviceInvokerSpendLimitRequest
	if !r.BindJSON(&req) {
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
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	scopeKey := strings.TrimSpace(req.ScopeKey)
	if scopeKey == "" {
		scopeKey = strings.TrimSpace(req.RoleID)
	}
	if err := svc.SetInvokerSpendLimits(r.Request.Context(), *payer, billingservice.InvokerSpendLimitInput{
		Scope:    req.Scope,
		ScopeKey: scopeKey,
		Windows:  spendLimitWindowInputs(req.Windows),
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set subject budget policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceGetInvokerSpendLimits returns ONLY the subject-owned budget-scope
// policies (#473) for host reconciliation — platform-owned caps are invisible.
// customer_id is a query param; the merchant is pinned from the API key.
// Operator API key, credits:read.
func ServiceGetInvokerSpendLimits(r *httprequest.Request) {
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
	policies, err := svc.InvokerSpendLimits(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "subject budget policies lookup failed")
		return
	}
	out := make([]map[string]any, 0, len(policies))
	for _, p := range policies {
		ws := make([]spendLimitWindow, 0, len(p.Windows))
		for _, w := range p.Windows {
			ws = append(ws, spendLimitWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit})
		}
		row := map[string]any{"scope": p.Scope, "scope_key": p.ScopeKey, "windows": ws}
		if strings.EqualFold(p.Scope, "role") {
			row["role_id"] = p.ScopeKey
		}
		out = append(out, row)
	}
	r.SuccessJSON(map[string]any{"policies": out})
}
