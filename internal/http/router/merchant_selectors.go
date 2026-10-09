package router

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// merchantScoped are the route groups, beneath the API root, that act on one
// merchant's books under a credential.
var merchantScoped = []string{"/v1/admin", "/v1/merchant", "/v1/me"}

// ResolveMerchantSelectors makes every merchant-scoped route in the table
// honor the OpenRails-Merchant selector. A request that names a merchant has
// it resolved and pinned before the route authorizes its credential, which
// must be bound to that same merchant; a request that names none is served
// as its credential and deployment resolve it. extra lists further prefixes
// that serve merchant-scoped routes (customer routes at a host's own path).
func ResolveMerchantSelectors(table *Table, prefix string, resolve func(context.Context, *http.Request) (billingauth.Target, error), extra ...string) {
	groups := make([]string, 0, len(merchantScoped)+len(extra))
	for _, group := range merchantScoped {
		groups = append(groups, prefix+group)
	}
	groups = append(groups, extra...)
	for i, entry := range table.Entries {
		if entry.Method == http.MethodOptions || !under(entry.Path, groups) {
			continue
		}
		next := entry.Handler
		table.Entries[i].Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, present, err := merchant.ParseSelector(r.Header)
			if err != nil {
				refuse(w, billingauth.Refusal(billing.CodeMerchantSelectorInvalid))
				return
			}
			if !present {
				next.ServeHTTP(w, r)
				return
			}
			if resolve == nil {
				refuse(w, billingauth.Refusal(billing.CodeMerchantDirectoryUnavailable))
				return
			}
			target, err := resolve(r.Context(), r)
			if err != nil {
				var refusal billingauth.GateError
				if !errors.As(err, &refusal) {
					refusal = billingauth.Refusal(billing.CodeMerchantDirectoryUnavailable)
				}
				refuse(w, refusal)
				return
			}
			next.ServeHTTP(w, r.WithContext(merchanttarget.WithResolved(merchant.WithID(r.Context(), target.MerchantID), target)))
		})
	}
}

func under(path string, groups []string) bool {
	for _, group := range groups {
		if path == group || strings.HasPrefix(path, group+"/") {
			return true
		}
	}
	return false
}

func refuse(w http.ResponseWriter, refusal billingauth.GateError) {
	answer := billingauth.RefusalError(refusal)
	billingauth.WriteJSONError(w, answer.HTTPStatus, answer.Code, answer.Message)
}
