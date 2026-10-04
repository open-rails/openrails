package middleware

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// EnforceMerchantBinding checks assertions after authentication and before the
// principal is pinned. Neither a Client header nor an existing runtime/Host
// binding may silently select a merchant different from the verified one.
func EnforceMerchantBinding(r *request.Request, actual billing.MerchantID) bool {
	selector, present, err := merchant.ParseSelector(r.Request.Header)
	if err != nil {
		r.ErrorCode(billing.CodeMerchantSelectorInvalid, "")
		return false
	}
	if present && selector.Slug != "" {
		// A slug is only as good as its resolution: the route's selector
		// middleware resolved it, and the credential must be that merchant's.
		target, ok := merchanttarget.FromContext(r.Request.Context())
		if !ok || target.MerchantID != actual || merchanttarget.Assert(r.Request, target) != nil {
			r.ErrorCode(billing.CodeMerchantBindingMismatch, "resolved merchant binding mismatch")
			return false
		}
	}
	if present && !selector.ID.IsZero() && selector.ID != actual {
		r.ErrorCode(billing.CodeMerchantBindingMismatch, "merchant binding mismatch")
		return false
	}
	if bound, ok := merchant.FromContext(r.Request.Context()); ok && !bound.IsZero() && bound != actual {
		r.ErrorCode(billing.CodeMerchantBindingMismatch, "configured merchant binding mismatch")
		return false
	}
	return true
}
