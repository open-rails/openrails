package mandates

import "strings"

// optInCountries are the EEA and the UK, where keeping a card for later
// purchases needs the customer's unticked opt-in (EDPB Recommendations
// 02/2021); elsewhere it is disclosed.
var optInCountries = map[string]bool{
	"AT": true, "BE": true, "BG": true, "CY": true, "CZ": true, "DE": true, "DK": true, "EE": true, "ES": true, "FI": true,
	"FR": true, "GR": true, "HR": true, "HU": true, "IE": true, "IT": true, "LT": true, "LU": true, "LV": true, "MT": true,
	"NL": true, "PL": true, "PT": true, "RO": true, "SE": true, "SI": true, "SK": true,
	"IS": true, "LI": true, "NO": true, "GB": true,
}

// ReuseNeedsOptIn reports whether keeping a new card for one-click buys
// needs the customer's opt-in: judged by billing country, then card country.
// Neither known keeps the card.
func ReuseNeedsOptIn(billingCountry, cardCountry string) bool {
	country := strings.ToUpper(strings.TrimSpace(billingCountry))
	if country == "" {
		country = strings.ToUpper(strings.TrimSpace(cardCountry))
	}
	return optInCountries[country]
}

// KeepsNewCard is whether a new card is kept for one-click buys: the
// customer's choice when given, else kept unless the region needs an opt-in.
func KeepsNewCard(reusable *bool, billingCountry, cardCountry string) bool {
	if reusable != nil {
		return *reusable
	}
	return !ReuseNeedsOptIn(billingCountry, cardCountry)
}
