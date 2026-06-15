package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Self-service credit-ACCOUNT surface (issue #339 gap-fill): the authenticated
// tenant_subject reads and configures its OWN billing account — settings
// (billing mode, spend caps, auto-top-up) + balance/outstanding + transaction
// history. These mirror the service-side account-settings routes
// (service_credits.go, issue #242) but the subject is ALWAYS the delegated
// principal's subject (r.GetUser()), never caller-supplied: there is no
// customer_id parameter anywhere on this surface.
//
// The payer is resolved exactly like the rest of /v1/self
// (identity.CustomerIDFromString over the acting subject — see
// GetMyUsage/GetMyInvoices), and every query runs RLS-scoped to the pinned
// tenant.

// selfAccountPayer resolves the acting payer from the delegated principal, or
// writes the error response and returns false.
func selfAccountPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return identity.CustomerID(uuid.Nil), false
	}
	payer := identity.CustomerIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payer could not be resolved from subject")
		return identity.CustomerID(uuid.Nil), false
	}
	return payer, true
}

// selfAccountResponse is the caller's account view: the live balance snapshot
// (balance/held/available/outstanding + billing mode) plus the stored settings.
type selfAccountResponse struct {
	Currency              string `json:"currency"`
	BillingMode           string `json:"billing_mode"`
	BalanceAmount         int64  `json:"balance_amount"`
	HeldAmount            int64  `json:"held_amount"`
	AvailableAmount       int64  `json:"available_amount"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
	Settings              any    `json:"settings"`
}

// GetMyCreditAccount (GET /v1/self/account?currency=) returns the authenticated
// subject's account settings + balance/outstanding for one currency
// (issue #339). It reuses the service-side GetCreditAccount/settings logic,
// scoped to the delegated subject.
func GetMyCreditAccount(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	snap, err := svc.GetCreditAccount(r.Request.Context(), payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(selfAccountResponse{
		Currency:              snap.Currency,
		BillingMode:           snap.BillingMode,
		BalanceAmount:         snap.BalanceAmount,
		HeldAmount:            snap.HeldAmount,
		AvailableAmount:       snap.AvailableAmount,
		OutstandingOwedAmount: snap.OutstandingOwedAmount,
		Settings:              settings,
	})
}

// selfAccountSettingsRequest is the PUT body for configuring the caller's own
// credit account. It is the service-side serviceAccountSettingsRequest WITHOUT
// customer_id: the subject is always the delegated principal's subject.
type selfAccountSettingsRequest struct {
	Currency                 string  `json:"currency"`
	BillingMode              *string `json:"billing_mode"` // "prepaid" | "arrears"
	MaxSpendPerDay           *int64  `json:"max_spend_per_day"`
	MaxSpendPerMonth         *int64  `json:"max_spend_per_month"`
	MaxOutstandingOwedAmount *int64  `json:"max_outstanding_owed_amount"`
	LowBalanceThreshold      *int64  `json:"low_balance_threshold"`
	AutoTopupEnabled         *bool   `json:"auto_topup_enabled"`
	AutoTopupAmountCents     *int64  `json:"auto_topup_amount_cents"`
	AutoTopupPaymentMethod   *string `json:"auto_topup_payment_method_id"`
	DefaultCreditExpiryDays  *int    `json:"default_credit_expiry_days"`
	HardStopOnBreach         *bool   `json:"hard_stop_on_breach"`
	AlertThresholdPct        *int    `json:"alert_threshold_pct"`
}

// SetMyCreditAccountSettings (PUT /v1/self/account/settings) configures the
// authenticated subject's OWN billing account (issue #339): billing mode,
// spend caps, auto-top-up, expiry default. Gated by the dedicated
// openrails:self:billing:write permission (read tokens cannot reconfigure).
// Returns the stored settings after the update, like the service-side route.
func SetMyCreditAccountSettings(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	var req selfAccountSettingsRequest
	if !r.BindJSON(&req) {
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
		AutoTopupAmountCents:     req.AutoTopupAmountCents,
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
		in.AutoTopupPaymentMethod = &pm
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetCreditAccountSettings(r.Request.Context(), payer, currency, in); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	settings, err := svc.GetCreditAccountSettings(r.Request.Context(), payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(settings)
}

// GetMyAccountTransactions (GET /v1/self/account/transactions?currency=&limit=&offset=)
// lists the authenticated subject's transactions for one currency
// (issue #339), newest first. Same wire shape as the service-side route, scoped
// to the delegated subject.
func GetMyAccountTransactions(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.GetCustomerCreditTransactions(r.Request.Context(), payer, currency, limit, offset)
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
