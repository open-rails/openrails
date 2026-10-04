package hostedcheckout

import (
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

// Drivable reports whether the payment page can run driver: it has no
// billing client of the buyer's, so Stripe Elements card setup is not offered.
func Drivable(driver string) bool {
	switch driver {
	case "collect_js", "card", "redirect", "solana_pay":
		return true
	}
	return false
}

// TakesCards reports whether driver pays with a new or saved card.
func TakesCards(driver string) bool { return driver == "collect_js" || driver == "card" }

// Payment turns the browser's pay body into the engine's payment options for
// the minted option it names. The option decides the PSP and the settlement
// token; the browser supplies only the instrument and its billing identity.
// savedMethod reports whether a saved method id is the buyer's on this option.
func Payment(option Option, input billing.HostedCheckoutPayRequest, verifiedEmail string, savedMethod func(id string) bool) (billing.CheckoutPaymentOptions, error) {
	input = trimPayment(input)
	if exceedsPaymentLimits(input) {
		return billing.CheckoutPaymentOptions{}, ErrInvalid
	}
	if raw := input.PaymentMethodID; raw != "" {
		if id, err := billing.ParsePaymentMethodID(raw); err != nil || id.IsZero() {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
	}
	out := billing.CheckoutPaymentOptions{
		Rail: option.Selector, PSPID: option.PSPID,
		PaymentToken: input.PaymentToken, PaymentMethodID: input.PaymentMethodID,
		Email: verifiedEmail, NameOnCard: input.NameOnCard,
		Address1: input.Address1, City: input.City, State: input.State, Zip: input.Zip, Country: input.Country,
		LastFour: input.LastFour, CardType: input.CardType, ExpiryDate: input.ExpiryDate,
	}
	hasToken, hasMethod, hasCard := input.PaymentToken != "", input.PaymentMethodID != "", input.Card != nil
	switch option.Driver {
	case "collect_js", "card":
		// A collect_js page tokenizes the card; a card page (the PSP's
		// card_entry is server) sends it to OpenRails.
		newCard, other := hasToken, hasCard
		if option.Driver == "card" {
			newCard, other = hasCard, hasToken
		}
		if other || newCard == hasMethod {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
		if hasCard {
			// The card names itself; the engine stamps its display fields.
			out.Card, out.LastFour, out.CardType, out.ExpiryDate = input.Card, "", "", ""
		}
		// The compact card form collects name, country and postal code only.
		out.Address1, out.City, out.State = "", "", ""
		if hasMethod {
			if !savedMethod(input.PaymentMethodID) {
				return billing.CheckoutPaymentOptions{}, ErrInvalid
			}
			// The vaulted method owns its billing identity.
			out.NameOnCard, out.Zip, out.Country = "", "", ""
			out.LastFour, out.CardType, out.ExpiryDate = "", "", ""
			break
		}
		if input.NameOnCard == "" || !validCountry(input.Country) ||
			(postalRequired(input.Country) && input.Zip == "") || !validUSPostal(input.Country, input.Zip) {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
	case "redirect":
		if hasToken || hasMethod || hasCard {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
	case "solana_pay":
		if hasToken || hasMethod || hasCard {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
		bound := strings.ToUpper(strings.TrimSpace(option.PublicConfig["token_symbol"]))
		if bound == "" || (input.TokenSymbol != "" && !strings.EqualFold(input.TokenSymbol, bound)) {
			return billing.CheckoutPaymentOptions{}, ErrInvalid
		}
		out.TokenSymbol, out.Flow = bound, "transfer_request"
	default:
		return billing.CheckoutPaymentOptions{}, ErrInvalid
	}
	return out, nil
}

func trimPayment(in billing.HostedCheckoutPayRequest) billing.HostedCheckoutPayRequest {
	in.OptionID = strings.TrimSpace(in.OptionID)
	in.PaymentToken = strings.TrimSpace(in.PaymentToken)
	in.PaymentMethodID = strings.TrimSpace(in.PaymentMethodID)
	in.NameOnCard = strings.TrimSpace(in.NameOnCard)
	in.Address1 = strings.TrimSpace(in.Address1)
	in.City = strings.TrimSpace(in.City)
	in.State = strings.TrimSpace(in.State)
	in.Zip = strings.TrimSpace(in.Zip)
	in.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	in.TokenSymbol = strings.TrimSpace(in.TokenSymbol)
	in.LastFour = strings.TrimSpace(in.LastFour)
	in.CardType = strings.TrimSpace(in.CardType)
	in.ExpiryDate = strings.TrimSpace(in.ExpiryDate)
	return in
}

func exceedsPaymentLimits(in billing.HostedCheckoutPayRequest) bool {
	return len(in.OptionID) > 128 || len(in.PaymentToken) > 4096 || len(in.PaymentMethodID) > 64 ||
		utf8.RuneCountInString(in.NameOnCard) > 200 || len(in.Address1) > 200 ||
		len(in.City) > 100 || len(in.State) > 100 || len(in.Zip) > 32 || len(in.Country) > 3 ||
		len(in.TokenSymbol) > 16 || len(in.LastFour) > 4 || len(in.CardType) > 32 || len(in.ExpiryDate) > 7
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
