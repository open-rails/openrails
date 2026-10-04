package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/cardguard"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// cardFieldAdmitted applies the request-level rules for the typed `card` field
// (#1129), writing the refusal when one fails: never beside a payment_token,
// never over plain HTTP in live posture, and no card number in any other
// field. Whether the PSP takes cards on the server is decided where the PSP
// is resolved.
func cardFieldAdmitted(r *httprequest.Request, hasToken bool, others ...string) bool {
	if hasToken {
		r.ErrorJSON(http.StatusBadRequest, paymentmethods.ErrCardWithToken.Error())
		return false
	}
	if live := r.State == nil || r.State.Config == nil || !r.State.Config.IsTestMode(); live && !r.SecureTransport() {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "card_requires_https", "a card is accepted only over HTTPS"))
		return false
	}
	for _, value := range others {
		if cardguard.ContainsPAN(value) {
			r.ErrorJSON(http.StatusBadRequest, "a card number is accepted only in the card field")
			return false
		}
	}
	return true
}

func optionalStrings(values ...*string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != nil {
			out = append(out, *value)
		}
	}
	return out
}

// writeCardEntryError renders the refusals of a card OpenRails received
// itself; false means err is not one.
func writeCardEntryError(r *httprequest.Request, err error) bool {
	switch {
	case errors.Is(err, paymentmethods.ErrCardEntryNotEnabled):
		r.ErrorJSON(http.StatusBadRequest, paymentmethods.ErrCardEntryNotEnabled.Error())
	case errors.Is(err, paymentmethods.ErrCardWithToken):
		r.ErrorJSON(http.StatusBadRequest, paymentmethods.ErrCardWithToken.Error())
	case errors.Is(err, paymentmethods.ErrCardNotSaved):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeAPI, "card_not_saved", "The card was not saved. Enter it again."))
	default:
		return false
	}
	return true
}
