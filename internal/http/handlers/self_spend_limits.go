package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// The delegated invoker's own spend-window read (or#930). OpenRails enforces
// per-invoker spend windows on every admission; before this, the only signal an
// invoker got about its budget was a denial at submit time. This returns the
// windows it is actually metered against, with their live totals — a read over
// the accounting admission already keeps, not a second one.

// GetMySpendLimits (GET /v1/me/spend-limits?currency=) returns the spend windows
// the AUTHENTICATED invoker is enforced against, with live used/reserved/
// remaining and the window's real reset boundary.
//
// SELF-SCOPED BY CONSTRUCTION: both coordinates — the payer account and the
// invoker — come from the resolved principal, never from the request. There is
// no addressing on this route, and naming another subject is refused rather than
// ignored (a silently-ignored parameter reads to the caller as a successful
// cross-read). The delegations a customer has granted are the merchant's
// /v1/admin/customers/{customer_id}/spend-delegations.
func GetMySpendLimits(r *httprequest.Request) {
	if addressed := addressedSpendScope(r); addressed != "" {
		r.APIError(api.Coded(billing.CodeInvalidQuery, addressed+" is not accepted: /v1/me/spend-limits answers only for the authenticated invoker").WithParam(addressed))
		return
	}
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	// The invoker reads its own windows; a customer acting itself is its own
	// invoker.
	invoker := scope.Invoker()
	if invoker == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "invoker could not be resolved from the credential")
		return
	}
	var q currencyQuery
	if !r.BindQuery(&q) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	windows, err := svc.InvokerSpendWindows(r.Request.Context(), payer, billingservice.InvokerSpendWindowsInput{
		Invoker:  invoker,
		Currency: q.Currency,
	})
	if err != nil {
		writeMoneyError(r, err, "spend window lookup failed")
		return
	}
	r.SuccessJSON(billing.SpendLimits{Currency: money.NormalizeCurrency(q.Currency), Invoker: invoker, Windows: windows})
}

// addressedSpendScope names the first cross-subject addressing parameter present
// on the request, or "" when the caller asked only about itself.
func addressedSpendScope(r *httprequest.Request) string {
	query := r.Request.URL.Query()
	for _, key := range []string{"invoker", "customer_id", "scope_key", "subject"} {
		if query.Has(key) {
			return key
		}
	}
	return ""
}
