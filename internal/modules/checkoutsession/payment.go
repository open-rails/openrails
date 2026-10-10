package checkoutsession

import (
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
)

// Drivable reports whether the payment page can run driver. A
// stripe_elements option pays with a card the buyer saved in Stripe's own
// fields, which only a page holding the buyer's billing client can enter.
func Drivable(driver string) bool {
	switch driver {
	case "collect_js", "card", "stripe_elements", "redirect", "solana_pay":
		return true
	}
	return false
}

// TakesCards reports whether driver pays with a new or saved card.
func TakesCards(driver string) bool {
	return driver == "collect_js" || driver == "card" || driver == "stripe_elements"
}

// Payment turns the browser's pay body into the engine's payment options for
// the minted option it names: the option decides the PSP and settlement token,
// the browser supplies only the instrument and billing identity. savedMethod
// reports whether a saved method id is the buyer's. A card the page sent is
// returned beside the options and travels to the engine only.
func Payment(option Option, input PayCheckoutSessionParams, verifiedEmail string, savedMethod func(id string) bool) (billing.CheckoutPaymentOptions, *cardguard.Card, error) {
	in := billingInputOf(input.BillingDetails)
	input.OptionID, input.PaymentToken, input.TokenSymbol = strings.TrimSpace(input.OptionID), strings.TrimSpace(input.PaymentToken), strings.TrimSpace(input.TokenSymbol)
	if len(input.OptionID) > 128 || len(input.PaymentToken) > 4096 || len(input.TokenSymbol) > 16 || in.exceedsLimits() {
		return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
	}
	var card *cardguard.Card
	out := billing.CheckoutPaymentOptions{PSP: option.Selector, PaymentToken: input.PaymentToken, PaymentMethodID: input.PaymentMethodID}
	hasToken, hasMethod, hasCard := input.PaymentToken != "", !input.PaymentMethodID.IsZero(), input.Card != nil
	switch option.Driver {
	case "collect_js", "card":
		// A collect_js page tokenizes the card; a card page (the PSP's
		// card_entry is server) sends it to OpenRails.
		newCard, other := hasToken, hasCard
		if option.Driver == "card" {
			newCard, other = hasCard, hasToken
		}
		if other || newCard == hasMethod {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
		if hasCard {
			card = input.Card
		}
		if hasMethod {
			if !savedMethod(input.PaymentMethodID.String()) {
				return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
			}
			// The vaulted method owns its billing identity.
			in = billingInput{}
			break
		}
		if in.name == "" || !validCountry(in.country) ||
			(postalRequired(in.country) && in.postal == "") || !validUSPostal(in.country, in.postal) {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
		// The compact card form collects name, country and postal code only.
		in = billingInput{name: in.name, postal: in.postal, country: in.country}
	case "stripe_elements":
		// The card was saved in Stripe's fields; the page names it.
		if hasToken || hasCard || !hasMethod || !savedMethod(input.PaymentMethodID.String()) {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
		in = billingInput{}
	case "redirect":
		if hasToken || hasMethod || hasCard {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
	case "solana_pay":
		if hasToken || hasMethod || hasCard {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
		bound := strings.ToUpper(strings.TrimSpace(option.PublicConfig["token_symbol"]))
		if bound == "" || (input.TokenSymbol != "" && !strings.EqualFold(input.TokenSymbol, bound)) {
			return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
		}
		out.TokenSymbol, out.Flow = bound, "transfer_request"
		in = billingInput{}
	default:
		return billing.CheckoutPaymentOptions{}, nil, ErrInvalid
	}
	// The email is the buyer's verified one, never the page's.
	in.email = strings.TrimSpace(verifiedEmail)
	out.BillingDetails = in.details()
	return out, card, nil
}

// billingInput is a pay body's billing details, trimmed.
type billingInput struct {
	name, email, phone, line1, line2, city, state, postal, country string
}

func billingInputOf(d *billing.BillingDetails) billingInput {
	text := func(v *string) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(*v)
	}
	var in billingInput
	if d == nil {
		return in
	}
	in.name, in.phone = text(d.Name), text(d.Phone)
	if a := d.Address; a != nil {
		in.line1, in.line2, in.city, in.state = text(a.Line1), text(a.Line2), text(a.City), text(a.State)
		in.postal, in.country = text(a.PostalCode), strings.ToUpper(text(a.Country))
	}
	return in
}

func (in billingInput) exceedsLimits() bool {
	return utf8.RuneCountInString(in.name) > 200 || len(in.phone) > 32 || len(in.line1) > 200 || len(in.line2) > 200 ||
		len(in.city) > 100 || len(in.state) > 100 || len(in.postal) > 32 || len(in.country) > 3
}

// details is the input as billing details, nil when empty.
func (in billingInput) details() *billing.BillingDetails {
	value := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	out := &billing.BillingDetails{Name: value(in.name), Email: value(in.email), Phone: value(in.phone)}
	address := &billing.BillingAddress{Line1: value(in.line1), Line2: value(in.line2), City: value(in.city), State: value(in.state),
		PostalCode: value(in.postal), Country: value(in.country)}
	if *address != (billing.BillingAddress{}) {
		out.Address = address
	}
	if *out == (billing.BillingDetails{}) {
		return nil
	}
	return out
}

func validCountry(country string) bool {
	return len(country) == 2 && country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z'
}

// Kept in step with billing-ui's POSTAL_OPTIONAL_COUNTRIES: a postal code is
// not demanded there, and still forwarded when given.
const postalOptionalCountries = " AE AG AO AQ AW BF BI BJ BO BQ BS BV BW BZ CF CG CI CK CM CW DM ER FJ GA GD GM GQ JM KM KP LY ML MR QA RW SB SC SL SO SR SS ST SX SY TD TG TK TO TV UM VU YE ZW "

func postalRequired(country string) bool {
	return !strings.Contains(postalOptionalCountries, " "+country+" ")
}

func validUSPostal(country, postal string) bool {
	if country != "US" || postal == "" {
		return true
	}
	if len(postal) != 5 && len(postal) != 10 {
		return false
	}
	for i, c := range postal {
		if i == 5 {
			if c != '-' {
				return false
			}
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
