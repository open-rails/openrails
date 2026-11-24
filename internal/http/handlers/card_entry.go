package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/cardguard"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// refuseCardNumber writes the PAN firewall's refusal of a request body with a
// card number in any field but the typed `card`, and reports whether it did.
func refuseCardNumber(r *httprequest.Request, body any) bool {
	field := cardguard.PANField(body)
	if field == "" {
		return false
	}
	r.APIError(api.Coded(billing.CodeInvalidParam, "a card number is accepted only in a card field").WithParam(field))
	return true
}

// cardFieldAdmitted applies the request-level rules for the typed `card` field,
// writing the refusal when one fails: never beside a payment_token, and never
// over plain HTTP unless the merchant has no live card PSP.
// Whether the PSP takes cards on the server is decided where the PSP is
// resolved.
func cardFieldAdmitted(r *httprequest.Request, hasToken bool) bool {
	if hasToken {
		r.ErrorCode(billing.CodeInvalidParam, paymentmethods.ErrCardWithToken.Error())
		return false
	}
	if !r.SecureTransport() && !noLivePSP(r) {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeCardRequiresHTTPS, "a card is accepted only over HTTPS"))
		return false
	}
	return true
}

// noLivePSP reports whether the request's merchant has no live card PSP.
func noLivePSP(r *httprequest.Request) bool {
	if r.State == nil {
		return false
	}
	probe := livePSPProbe(r)
	if probe == nil {
		return false
	}
	presence, err := probe(r.Request.Context())
	return err == nil && presence == merchants.LiveRailAbsent
}

// writeCardEntryError renders the refusals of a card OpenRails received
// itself; false means err is not one.
func writeCardEntryError(r *httprequest.Request, err error) bool {
	switch {
	case errors.Is(err, paymentmethods.ErrCardEntryNotEnabled):
		r.ErrorCode(billing.CodeInvalidParam, paymentmethods.ErrCardEntryNotEnabled.Error())
	case errors.Is(err, paymentmethods.ErrCardWithToken):
		r.ErrorCode(billing.CodeInvalidParam, paymentmethods.ErrCardWithToken.Error())
	case errors.Is(err, paymentmethods.ErrCardNotSaved):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeAPI, billing.CodeCardNotSaved, "The card was not saved. Enter it again."))
	default:
		return false
	}
	return true
}
