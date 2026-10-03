package embedhttp

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

func nativeTreasury(authenticate router.Middleware, auth *billingauth.Integration) router.Middleware {
	return func(next router.Handler) router.Handler {
		return authenticate(func(r *request.Request) {
			identity, err := authenticateIntegration(r.Request.Context(), r.Request, auth)
			target, ok := merchanttarget.FromContext(r.Request.Context())
			if err != nil || !ok || identity.Kind != billingauth.NativeUser || identity.CustomerID == "" {
				r.AbortJSON(http.StatusUnauthorized, "native customer identity required")
				return
			}
			authority := middleware.NativeTreasuryAuthority{CustomerID: identity.CustomerID, Target: target}
			if auth.Authorization != nil {
				authority.Authorize = func(ctx context.Context, permission string, payer billingauth.Target) error {
					return auth.Authorization.Authorize(ctx, r.Request, identity, billingauth.Requirement{Scope: billingauth.CustomerScope, Permission: permission, Target: payer})
				}
			}
			if auth.RecentSignIn != nil {
				authority.RecentSignIn = func(ctx context.Context) error { return auth.RecentSignIn.CheckRecentSignIn(ctx, r.Request) }
			}
			middleware.SetNativeTreasuryAuthority(r, authority)
			next(r)
		})
	}
}
