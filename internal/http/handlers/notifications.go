package handlers

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// customerScope is the merchant and customer a /me request acts for.
func customerScope(r *httprequest.Request) (uuid.UUID, uuid.UUID, bool) {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return uuid.Nil, uuid.Nil, false
	}
	return scope.Merchant().UUID(), scope.Customer().UUID(), true
}
