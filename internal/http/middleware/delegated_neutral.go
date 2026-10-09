package middleware

import (
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/http/request"
)

// ResourceUserContextKey holds the *credential.ResourceUser a trusted
// issuer's token resolved to on a signed-in user's own control-plane routes.
const ResourceUserContextKey = "openrails.resource_user"

// ResourceUserFromRequest returns the trusted issuer's principal the user
// tier resolved, if any.
func ResourceUserFromRequest(r *request.Request) (*credential.ResourceUser, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Get(ResourceUserContextKey)
	if !ok {
		return nil, false
	}
	user, ok := v.(*credential.ResourceUser)
	return user, ok && user != nil
}
