package handlers

import (
	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// GetCurrencies serves the public currency registry: the scale every monetary
// string on this deployment's wire is expressed in.
func GetCurrencies(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "public, max-age=3600")
	r.SuccessJSON(openrails.CurrencyRegistry{Object: "currencies", Currencies: openrails.Currencies()})
}
