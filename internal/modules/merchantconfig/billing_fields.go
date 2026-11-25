package merchantconfig

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// DefaultBillingFields are Stripe's: the country always, the postal code in
// the US alone.
var DefaultBillingFields = billing.BillingFieldSettings{Country: billing.BillingFieldRequired, PostalCode: billing.BillingFieldAuto}

// NormalizeBillingFields validates checkout.billing_fields, filling in the
// defaults.
func NormalizeBillingFields(in *billing.BillingFieldSettings) (billing.BillingFieldSettings, error) {
	out := DefaultBillingFields
	if in == nil {
		return out, nil
	}
	country := billing.BillingFieldMode(strings.TrimSpace(string(in.Country)))
	switch country {
	case "":
	case billing.BillingFieldRequired, billing.BillingFieldOptional, billing.BillingFieldHidden:
		out.Country = country
	default:
		return out, fmt.Errorf("checkout.billing_fields.country must be required, optional or hidden")
	}
	postal := billing.BillingFieldMode(strings.TrimSpace(string(in.PostalCode)))
	switch postal {
	case "":
	case billing.BillingFieldAuto, billing.BillingFieldRequired, billing.BillingFieldOptional, billing.BillingFieldHidden:
		out.PostalCode = postal
	default:
		return out, fmt.Errorf("checkout.billing_fields.postal_code must be auto, required, optional or hidden")
	}
	return out, nil
}

// BillingFields are the billing fields settings name; the defaults when they
// name none, or name them invalidly (such settings are never served).
func BillingFields(settings billing.MerchantSettings) billing.BillingFieldSettings {
	if settings.Checkout == nil {
		return DefaultBillingFields
	}
	out, err := NormalizeBillingFields(settings.Checkout.BillingFields)
	if err != nil {
		return DefaultBillingFields
	}
	return out
}

// BillingFields are the billing fields of the merchant in context; the
// defaults when no configuration is bound.
func (s *Store) BillingFields(ctx context.Context) (billing.BillingFieldSettings, error) {
	settings, err := s.Settings(ctx)
	if errors.Is(err, ErrNoSettings) {
		return DefaultBillingFields, nil
	}
	return settings.Config.BillingFields, err
}

// AdmitBillingDetails applies fields to a new card's billing details: a
// required one that is missing is refused (billing_details_required, its param
// under prefix), a hidden one is dropped. It answers the details to keep,
// never d itself.
func AdmitBillingDetails(fields billing.BillingFieldSettings, d *billing.BillingDetails, prefix string) (*billing.BillingDetails, error) {
	out := &billing.BillingDetails{}
	if d != nil {
		*out = *d
	}
	address := &billing.BillingAddress{}
	if out.Address != nil {
		*address = *out.Address
	}
	country := strings.ToUpper(text(address.Country))
	if fields.Country == billing.BillingFieldHidden {
		address.Country, country = nil, ""
	}
	if fields.Country == billing.BillingFieldRequired && country == "" {
		return nil, missingBillingDetail(prefix+".address.country", "Select the card's billing country.")
	}
	switch fields.PostalCode {
	case billing.BillingFieldHidden:
		address.PostalCode = nil
	case billing.BillingFieldRequired:
		if text(address.PostalCode) == "" {
			return nil, missingBillingDetail(prefix+".address.postal_code", "Enter the card's billing postal code.")
		}
	case billing.BillingFieldAuto:
		if country == "US" && text(address.PostalCode) == "" {
			return nil, missingBillingDetail(prefix+".address.postal_code", "Enter the card's billing ZIP code.")
		}
	}
	out.Address = address
	if *address == (billing.BillingAddress{}) {
		out.Address = nil
	}
	if *out == (billing.BillingDetails{}) {
		return nil, nil
	}
	return out, nil
}

func missingBillingDetail(param, message string) error {
	return apperr.New(http.StatusBadRequest, billing.CodeBillingDetailsRequired, message).WithParam(param)
}

func text(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}
