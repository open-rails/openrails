package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	billingservice "github.com/open-rails/openrails/internal/service"
)

const (
	codePaymentMethodNotUsable = "payment_method_not_usable"
	editPaymentMethodTimeout   = 25 * time.Second
)

// UpdatePaymentMethod (PATCH /me/payment-methods/{id}) edits a saved card in
// place: its expiry and billing details, pushed to its holder, and whether it
// is kept for one-click buys. The card number never changes.
func UpdatePaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	var body billing.UpdatePaymentMethodParams
	if !r.BindJSON(&body) {
		return
	}
	methodID, err := billing.ParsePaymentMethodID(r.Param("id"))
	if err != nil || methodID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment method id").WithParam("id"))
		return
	}
	if (body.ExpMonth == nil) != (body.ExpYear == nil) {
		r.APIError(api.Coded(billing.CodeInvalidParam, "exp_month and exp_year change together").WithParam("exp_month"))
		return
	}
	if body.ExpMonth != nil {
		month, year := *body.ExpMonth, *body.ExpYear
		now := r.Clock.Now().UTC()
		if month < 1 || month > 12 || year < 2000 || year > 2199 || year < now.Year() || year == now.Year() && month < int(now.Month()) {
			r.APIError(api.Coded(billing.CodeInvalidParam, "exp_month and exp_year must name a month not yet past").WithParam("exp_month"))
			return
		}
	}
	if !cardFieldAdmitted(r, false, billingDetailStrings(body.BillingDetails)...) {
		return
	}
	customer := identity.CustomerIDFromString(user.ID)
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	ctx, cancel := r.Budget(editPaymentMethodTimeout)
	defer cancel()
	if err := svc.EditPaymentMethod(ctx, customer.UUID(), methodID.UUID(), body); err != nil {
		writePaymentMethodChangeError(r, err)
		return
	}
	writeOwnedPaymentMethod(r, customer, methodID)
}

func writeOwnedPaymentMethod(r *httprequest.Request, customer identity.CustomerID, methodID billing.PaymentMethodID) {
	pm, ok := ownedPaymentMethod(r, methodID.UUID(), customer.String())
	if !ok {
		return
	}
	writePaymentMethod(r, http.StatusOK, customer, pm)
}

func writePaymentMethodChangeError(r *httprequest.Request, err error) {
	var refused *paymentmethods.PaymentMethodError
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
	case errors.Is(err, billingservice.ErrPaymentMethodNotUsable):
		r.ErrorCode(codePaymentMethodNotUsable, "This payment method is closed, replaced, removed or being deleted.")
	case errors.Is(err, billingservice.ErrPaymentMethodEditUnsupported):
		r.ErrorCode(codePaymentMethodUpdateUnsupported, err.Error())
	case errors.Is(err, billingservice.ErrPaymentMethodEditRefused):
		r.ErrorCode(billing.CodePaymentProviderRejected, "The payment provider refused the change; the card is unchanged.")
	case errors.As(err, &refused):
		writePaymentMethodError(r, refused)
	case errors.Is(err, merchants.ErrConfigUnavailable), errors.Is(err, paymentmethods.ErrPaymentMethodProviderUnavailable):
		r.ErrorCode(billing.CodeServiceUnavailable, "Payment rail credentials are temporarily unavailable")
	default:
		if ambiguous := createPaymentMethodProviderError(err); ambiguous != nil {
			r.APIError(ambiguous)
			return
		}
		r.InternalError("Failed to change payment method", err)
	}
}
