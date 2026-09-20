package middleware

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// EnforceMerchantBinding checks assertions after authentication and before the
// principal is pinned. Neither a Client header nor an existing runtime/Host
// binding may silently select a merchant different from the verified one.
func EnforceMerchantBinding(r *request.Request, actual merchant.ID) bool {
	if raw := strings.TrimSpace(r.Header(merchant.BindingHeader)); raw != "" {
		expected, err := merchant.ParseID(raw)
		if err != nil || expected.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid merchant binding")
			return false
		}
		if expected != actual {
			r.APIError(api.ConflictError("merchant binding mismatch"))
			return false
		}
	}
	if bound, ok := merchant.FromContext(r.Request.Context()); ok && !bound.IsZero() && bound != actual {
		r.APIError(api.ConflictError("configured merchant binding mismatch"))
		return false
	}
	return true
}
