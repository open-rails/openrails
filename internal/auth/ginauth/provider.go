// Package ginauth holds the gin middleware (Required/Optional) for the
// AuthKit-backed authenticator. It is imported only by the standalone (gin) HTTP
// path. The gin-free Authenticator core lives in internal/auth, so that
// gin-free importers (ultimately pkg/embedded) do not transitively pull in
// github.com/gin-gonic/gin (#285).
package ginauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/auth"
	provauth "github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// authKitProvider adapts the gin-free auth.Authenticator into the gin
// provauth.Provider mounted on OpenRails' standalone gin routes.
type authKitProvider struct {
	authenticator *auth.Authenticator
}

// NewProvider builds the gin provider around an already-constructed verifier.
func NewProvider(verifier auth.Verifier) provauth.Provider {
	return &authKitProvider{authenticator: auth.NewAuthenticator(verifier)}
}

// Authenticate forwards to the gin-free authenticator so this provider also
// satisfies billingauth.Authenticator (recoverable via provauth.AsAuthenticator
// for the neutral route path, issue #282).
func (p *authKitProvider) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if p == nil || p.authenticator == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	return p.authenticator.Authenticate(ctx, r)
}

func (p *authKitProvider) Required() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.authenticator == nil {
			log.Warn("auth middleware misconfigured: no authenticator provided")
			response.InternalError(c, "authentication disabled")
			c.Abort()
			return
		}

		uc, err := p.authenticator.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			if errors.Is(err, billingauth.ErrUnauthenticated) {
				response.UnauthorizedWithMessage(c, "authorization header required")
			} else {
				response.UnauthorizedWithMessage(c, auth.FormatVerifierError(err))
			}
			c.Abort()
			return
		}
		if verr := uc.ValidateSubject(); verr != nil {
			response.UnauthorizedWithMessage(c, verr.Error())
			c.Abort()
			return
		}

		c.Set("openrails.user_context", uc)
		c.Request = c.Request.WithContext(billingauth.SetUserContext(c.Request.Context(), uc))
		c.Next()
	}
}

func (p *authKitProvider) Optional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.authenticator == nil {
			c.Next()
			return
		}

		uc, err := p.authenticator.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			if !errors.Is(err, billingauth.ErrUnauthenticated) {
				log.WithError(err).Debug("optional jwt verification failed")
			}
			c.Next()
			return
		}
		if verr := uc.ValidateSubject(); verr != nil {
			log.WithError(verr).Debug("optional auth: non-UUID subject treated as anonymous")
			c.Next()
			return
		}

		c.Set("openrails.user_context", uc)
		c.Request = c.Request.WithContext(billingauth.SetUserContext(c.Request.Context(), uc))
		c.Next()
	}
}
