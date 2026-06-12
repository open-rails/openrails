package ginauth

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"

	"github.com/open-rails/openrails/pkg/billingauth"
)

// ginUserContextKey is the gin-context key under which the authenticated
// UserContext is stored; it MUST match UserContextFromGin.
const ginUserContextKey = "billing.user_context"

// ProviderFromAuthenticator adapts a framework-neutral billingauth.Authenticator
// into the gin Provider OpenRails mounts on its CURRENT gin routes. This is the
// transitional bridge: a host can bring its own auth today (implement
// billingauth.Authenticator) before the route layer is migrated to net/http
// (issue #282), after which billingauth.Required/Optional replace it. The
// adapter owns the 401/anonymous decision and stores the UserContext in both the
// gin context and the request context.
func ProviderFromAuthenticator(a billingauth.Authenticator) Provider {
	return &authenticatorProvider{a: a}
}

type authenticatorProvider struct {
	a billingauth.Authenticator
}

// Authenticate exposes the wrapped Authenticator so this Provider satisfies
// billingauth.Authenticator too, letting AsAuthenticator recover it for the
// gin-free embedded surface (issue #282).
func (p *authenticatorProvider) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if p == nil || p.a == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	return p.a.Authenticate(ctx, r)
}

// AsAuthenticator recovers a framework-neutral billingauth.Authenticator from a
// Provider when the provider implementation supports one (issue #282). Both the
// AuthKit-backed provider and ProviderFromAuthenticator do. It returns
// (nil, false) for providers that only expose gin middleware, so callers can
// fall back gracefully.
func AsAuthenticator(p Provider) (billingauth.Authenticator, bool) {
	if a, ok := p.(billingauth.Authenticator); ok && a != nil {
		return a, true
	}
	return nil, false
}

func (p *authenticatorProvider) Required() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.a == nil {
			response.InternalError(c, "authentication disabled")
			c.Abort()
			return
		}
		uc, err := p.a.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			response.UnauthorizedWithMessage(c, billingauth.UnauthenticatedMessage(err))
			c.Abort()
			return
		}
		if verr := uc.ValidateSubject(); verr != nil {
			response.UnauthorizedWithMessage(c, verr.Error())
			c.Abort()
			return
		}
		applyGinUserContext(c, uc)
		c.Next()
	}
}

func (p *authenticatorProvider) Optional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.a == nil {
			c.Next()
			return
		}
		if uc, err := p.a.Authenticate(c.Request.Context(), c.Request); err == nil && uc.ValidateSubject() == nil {
			applyGinUserContext(c, uc)
		}
		c.Next()
	}
}

// applyGinUserContext stores uc in BOTH the gin context and the request context,
// the single contract handlers rely on via UserContextFromGin.
func applyGinUserContext(c *gin.Context, uc billingauth.UserContext) {
	c.Set(ginUserContextKey, uc)
	c.Request = c.Request.WithContext(billingauth.SetUserContext(c.Request.Context(), uc))
}
