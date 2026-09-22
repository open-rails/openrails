package router

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AddMerchantSelectorRoutes registers the explicit-selector protocol as actual
// v2 operation routes. Older servers do not have these paths and therefore
// cannot silently execute a slug-targeted write in a credential's old book.
// The request URI is never rewritten: sender proof verification sees v2.
// Only the SDK's customer recovery and payment-authentication operations are included;
// browser-only /me routes are not duplicated as an extra surface.
func AddMerchantSelectorRoutes(table *Table, prefix string, resolve func(context.Context, *http.Request) (billingauth.Target, error)) {
	original := append([]Entry(nil), table.Entries...)
	for _, entry := range original {
		local := strings.TrimPrefix(entry.Path, prefix)
		selected := false
		for _, group := range []string{"/v1/merchant", "/v1/catalog", "/v1/import"} {
			if local == group || strings.HasPrefix(local, group+"/") {
				selected = true
				break
			}
		}
		switch entry.Method + " " + local {
		case "GET /v1/me/invoices/{id}", "POST /v1/me/invoices/{id}/pay-now", "GET /v1/me/subscriptions/{id}", "POST /v1/me/subscriptions/{id}/retry-now",
			"GET /v1/me/payment-operations/{id}/authentication", "POST /v1/me/payment-operations/{id}/authentication/confirm":
			selected = true
		}
		if !selected {
			continue
		}
		next := entry.Handler
		entry.Path = prefix + "/v2/" + strings.TrimPrefix(local, "/v1/")
		entry.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions && strings.TrimSpace(r.Header.Get(merchant.SlugHeader)) == "" && strings.TrimSpace(r.Header.Get(merchant.BindingHeader)) == "" {
				billingauth.WriteJSONError(w, http.StatusBadRequest, "merchant_selector_required", "explicit merchant selector required")
				return
			}
			if r.Method != http.MethodOptions {
				if resolve == nil {
					billingauth.WriteJSONError(w, 503, "authorization_unavailable", "merchant resolver unavailable")
					return
				}
				target, err := resolve(r.Context(), r)
				if err != nil {
					var gate billingauth.GateError
					if errors.As(err, &gate) {
						billingauth.WriteJSONError(w, gate.Status, "merchant_selection_invalid", gate.Message)
					} else {
						billingauth.WriteJSONError(w, 503, "merchant_directory_unavailable", "merchant directory unavailable")
					}
					return
				}
				r = r.WithContext(merchanttarget.WithResolved(merchant.WithID(r.Context(), target.MerchantID), target))
			}
			next.ServeHTTP(w, r)
		})
		table.Entries = append(table.Entries, entry)
	}
}
