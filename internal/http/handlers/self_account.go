package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Self-service money surface: the authenticated merchant_subject reads its own
// balance and transaction history and chooses its invoice collection method.
//
// The payer is resolved exactly like the rest of /v1/me
// (identity.CustomerIDFromString over the acting subject — see
// GetMyUsage/GetMyInvoices), and every query is scoped to the request
// merchant.

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

// GetMyBalance returns the customer's own money in one currency.
func GetMyBalance(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	getBalance(r, payer)
}

type CollectionPaymentMethodRequest struct {
	Currency        string                  `json:"currency"`
	PaymentMethodID billing.PaymentMethodID `json:"payment_method_id"`
}

type CollectionPaymentMethodResponse struct {
	Currency        string                  `json:"currency"`
	PaymentMethodID billing.PaymentMethodID `json:"payment_method_id"`
}

// SetMyCollectionPaymentMethod (PUT .../collection-payment-method) selects the
// payer's saved method for automatic invoice collection in one currency.
func SetMyCollectionPaymentMethod(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	var req CollectionPaymentMethodRequest
	if !r.BindJSON(&req) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	if err := money.RequireBillingCurrency(currency); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if req.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment_method_id")
		return
	}
	methodID := req.PaymentMethodID.UUID()
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetInvoiceCollectionPaymentMethod(r.Request.Context(), payer, currency, methodID); err != nil {
		if errors.Is(err, money.ErrCollectionPaymentMethodInvalid) {
			r.ErrorJSON(http.StatusBadRequest, "payment method is not eligible for invoice collection")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to set collection payment method")
		return
	}
	r.SuccessJSON(CollectionPaymentMethodResponse{Currency: currency, PaymentMethodID: req.PaymentMethodID})
}

// GetMyCreditTransactions lists the customer's own ledger in one currency.
func GetMyCreditTransactions(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listCreditTransactions(r, payer)
}
