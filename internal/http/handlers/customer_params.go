package handlers

import (
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	billingidentity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
)

// serviceCustomer converts a typed wire customer id to the engine's customer
// identity; the zero id is nil (absent).
func serviceCustomer(id billing.CustomerID) *billingidentity.CustomerID {
	if id.IsZero() {
		return nil
	}
	customer := billingidentity.CustomerID(id)
	return &customer
}

// customerIDParam reads a plain-UUID customer id from a path or query value;
// anything unparseable is the zero id, which callers refuse.
func customerIDParam(raw string) billing.CustomerID {
	id, err := billing.ParseCustomerID(raw)
	if err != nil {
		return billing.CustomerID{}
	}
	return id
}

// serviceRequiredCurrency answers a missing currency; a present one is
// normalized.
func serviceRequiredCurrency(r *httprequest.Request, raw string) (string, bool) {
	currency := strings.TrimSpace(raw)
	if currency == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "currency required").WithParam("currency"))
		return "", false
	}
	return money.NormalizeCurrency(currency), true
}
