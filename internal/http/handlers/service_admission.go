package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
	// AccrualRateDeltaPerHour is the or#897 PROSPECTIVE rate this request would
	// add, in micros per hour — "the VM I am about to start burns $2/hour". Only
	// the host knows it. Zero means the request adds no ongoing rate, which
	// leaves an accrual_rate_cap payer gated on what is already running.
	AccrualRateDeltaPerHour int64  `json:"accrual_rate_delta_per_hour,omitempty"`
	RequestID               string `json:"request_id"`
	Source                  string `json:"source"`
	ExpiresAt               *int64 `json:"expires_at"`
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

		AccrualRateDeltaPerHour: req.AccrualRateDeltaPerHour,
		Source:                  req.Source,
		SourceID:                req.RequestID,
		Roles:                   req.Roles,
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
		switch {
		case errors.Is(err, billingservice.ErrHoldDeadlineRequired):
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "expires_at required when estimated_amount places a hold"}
			continue
		case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "expires_at already passed"}
			continue
		}
		if err != nil {
			// th#1627: the wire string stays stable and non-leaky (it reaches the
			// host's tenant boundary), but the CAUSE must not be discarded — a 500
			// whose only content is the constant "admission check failed" cost a
			// cross-stack bisect to attribute. Operators read this log.
			log.WithError(err).WithFields(log.Fields{
				"customer_id": item.CustomerID,
				"invoker":     item.Invoker,
				"request_id":  item.RequestID,
				"source":      item.Source,
			}).Error("admission check failed")
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
		r.InternalError("billing service unavailable", err)
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
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	trustLevel, err := svc.GetTrustLevel(r.Request.Context(), *payer, currency)
	if err != nil {
		r.InternalError("trust level lookup failed", err)
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
		r.InternalError("billing service unavailable", err)
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
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.InternalError("report wasted spend failed", err)
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
		r.InternalError("billing service unavailable", err)
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
		r.InternalError("billing service unavailable", err)
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
	AutoTopupSafety            *billingservice.AutoTopupSafetyPolicy `json:"auto_topup_safety,omitempty"`
	Profile                    *serviceMerchantProfileConfiguration  `json:"profile,omitempty"`
	InvoiceCollectionThreshold *int64                                `json:"collection_threshold,omitempty"`
	InvoiceMonthlyFloor        *int64                                `json:"monthly_floor,omitempty"`
	InvoiceBillingBoundary     string                                `json:"billing_period_boundary,omitempty"`
	AlertEmail                 *string                               `json:"alert_email,omitempty"`
	// RepriceNoticeWindowDays (#781): the minimum advance-notice window (in
	// days) a subscription price INCREASE's effective_at must give existing
	// subscribers. Unset ⇒ subscriptions.DefaultRepriceNoticeWindowDays (30).
	RepriceNoticeWindowDays *int `json:"reprice_notice_window_days,omitempty"`
	// ArrearsGraceDays / ArrearsDelinquencyFloor (or#878): the delinquency
	// policy. Grace unset ⇒ delinquency.DefaultGraceDays (14); the floor unset ⇒
	// derived from monthly_floor.
	ArrearsGraceDays        *int   `json:"arrears_grace_days,omitempty"`
	ArrearsDelinquencyFloor *int64 `json:"arrears_delinquency_floor,omitempty"`
	// CheckoutRouting (or#288): the processor-routing policy. Omitted preserves
	// the stored policy; present replaces it whole (an empty array clears it
	// back to the built-in default order).
	CheckoutRouting     *[]models.CheckoutRoutingRule       `json:"checkout_routing,omitempty"`
	TrustLevelSchedules []serviceMerchantTrustLevelSchedule `json:"trust_level_schedules,omitempty"`
	// BillingPolicies / BillingPolicyBindings are the or#897 registry: named
	// policies, then the rungs that decide who gets which. Declared policies are
	// applied before bindings so a binding can name one from the same document.
	BillingPolicies                   []billingservice.BillingPolicyInput        `json:"billing_policies,omitempty"`
	BillingPolicyBindings             []billingservice.BillingPolicyBindingInput `json:"billing_policy_bindings,omitempty"`
	DelegatedInvokerWastedSpendLimits []serviceMerchantConfigWindow              `json:"delegated_invoker_wasted_spend_limits,omitempty"`

	// RetiredTrustLevelSpendLimits keeps the retired `trust_level_spend_limits`
	// key BOUND ONLY so a caller that still sends it is told what replaced it
	// (or#897 hard cut). A silently-ignored spend cap is a cap nobody enforced.
	RetiredTrustLevelSpendLimits []json.RawMessage `json:"trust_level_spend_limits,omitempty"`
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
	SignupURL   string `json:"signup_url,omitempty"`
}

// ServiceGetMerchantSettings returns the merchant-owned policy-sync document.
func ServiceGetMerchantSettings(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	cfg, _, err := svc.GetMerchantConfiguration(r.Request.Context())
	if err != nil {
		r.InternalError("get merchant settings failed", err)
		return
	}
	policies, err := svc.ListBillingPolicies(r.Request.Context())
	if err != nil {
		r.InternalError("get merchant settings failed", err)
		return
	}
	bindings, err := svc.ListBillingPolicyBindings(r.Request.Context())
	if err != nil {
		r.InternalError("get merchant settings failed", err)
		return
	}
	r.SuccessJSON(serviceMerchantSettingsRequest{
		Profile:                           merchantProfileResponsePtr(cfg.Profile),
		AutoTopupSafety:                   cfg.AutoTopupSafety,
		InvoiceCollectionThreshold:        cfg.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor:               cfg.InvoiceMonthlyFloor,
		InvoiceBillingBoundary:            cfg.InvoiceBillingBoundary,
		AlertEmail:                        cfg.AlertEmail,
		RepriceNoticeWindowDays:           cfg.RepriceNoticeWindowDays,
		ArrearsGraceDays:                  cfg.ArrearsGraceDays,
		ArrearsDelinquencyFloor:           cfg.ArrearsDelinquencyFloor,
		CheckoutRouting:                   cfg.CheckoutRouting,
		BillingPolicies:                   policies,
		BillingPolicyBindings:             bindings,
		DelegatedInvokerWastedSpendLimits: serviceMerchantConfigWindows(cfg.DelegatedInvokerWastedSpendWindows),
	})
}

// ServiceSetMerchantSettings installs the merchant-owned admission policy in
// one document: profile, trust-level schedules, billing policies + bindings,
// and delegated-invoker wasted-spend limits.
func ServiceSetMerchantSettings(r *httprequest.Request) {
	var req serviceMerchantSettingsRequest
	if !r.BindJSON(&req) {
		return
	}
	if len(req.RetiredTrustLevelSpendLimits) > 0 {
		r.ErrorJSON(http.StatusBadRequest, "trust_level_spend_limits was replaced by billing_policies + billing_policy_bindings (or#897): declare a named policy with kind outstanding_cap or window_spend_cap, then bind it to a tier or customer")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}

	// or#288: a malformed routing policy is a client error, not a server one.
	// Run the SAME normalizer the service will run, so the caller gets the
	// exact reason instead of a bare 500.
	if _, err := merchantconfig.AutoTopupSafety(req.AutoTopupSafety); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if req.CheckoutRouting != nil {
		if _, err := merchantconfig.NormalizeCheckoutRouting(*req.CheckoutRouting); err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
	}
	// Same contract for billing policies: validate the WHOLE document through the
	// shared normalizer before writing any of it, so a bad third policy cannot
	// leave the first two installed.
	for _, policy := range req.BillingPolicies {
		if _, _, err := billingservice.ValidateBillingPolicy(policy); err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
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
		AlertEmail:                         req.AlertEmail,
		RepriceNoticeWindowDays:            req.RepriceNoticeWindowDays,
		AutoTopupSafety:                    req.AutoTopupSafety,
		ArrearsGraceDays:                   req.ArrearsGraceDays,
		ArrearsDelinquencyFloor:            req.ArrearsDelinquencyFloor,
		CheckoutRouting:                    req.CheckoutRouting,
		DelegatedInvokerWastedSpendWindows: windows,
	}); err != nil {
		r.InternalError("set merchant settings failed", err)
		return
	}

	for _, schedule := range req.TrustLevelSchedules {
		currency, ok := serviceRequiredCurrency(r, schedule.Currency)
		if !ok {
			return
		}
		if err := moneyutil.ValidateCurrency(currency); err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if err := svc.SetTrustLevelSchedule(r.Request.Context(), billingidentity.CustomerID{}, currency, schedule.Schedule); err != nil {
			r.InternalError("set trust level schedule failed", err)
			return
		}
	}
	// Policies first: a binding in the same document may name one of them, and
	// the bindings FK refuses a name that does not exist yet.
	for _, policy := range req.BillingPolicies {
		if err := svc.SetBillingPolicy(r.Request.Context(), policy); err != nil {
			r.InternalError("set billing policy failed", err)
			return
		}
	}
	for _, binding := range req.BillingPolicyBindings {
		if err := svc.BindBillingPolicy(r.Request.Context(), binding); err != nil {
			if errors.Is(err, billingservice.ErrInvalidBillingPolicy) {
				r.ErrorJSON(http.StatusBadRequest, err.Error())
				return
			}
			r.InternalError("bind billing policy failed", err)
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
		SignupURL:   strings.TrimSpace(in.SignupURL),
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
		SignupURL:   in.SignupURL,
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
