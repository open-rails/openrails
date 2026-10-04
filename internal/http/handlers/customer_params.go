package handlers

import (
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	billingidentity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
)

// servicePayer converts a typed wire customer id to the engine's payer
// identity; the zero id is nil (absent).
func servicePayer(id billing.CustomerID) *billingidentity.CustomerID {
	if id.IsZero() {
		return nil
	}
	payer := billingidentity.CustomerID(id)
	return &payer
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

func parseServiceCustomerID(raw string) (*billingidentity.CustomerID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return nil, errors.New("invalid customer_id")
	}
	customer := billingidentity.CustomerID(id)
	return &customer, nil
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
