// Package ginauth holds the gin-typed slice of the auth boundary: the gin
// Provider middleware interface, the Authenticator→Provider bridge, and the
// gin-context UserContext helpers. It is imported only by the gin (standalone)
// HTTP path. The gin-free embedded surface depends on pkg/billingauth (and the
// gin-free re-exports in pkg/authprovider) instead, so that pkg/embedded does
// not transitively pull in github.com/gin-gonic/gin (#285).
package ginauth

import (
	"github.com/gin-gonic/gin"
)

// Provider is the app-facing auth boundary for the user/admin JWT verification
// middleware. It only VERIFIES the first-party credential; minting delegated /
// service credentials is the control plane's job.
//
// The middleware must set the user context in the Gin context using:
//
//	c.Set("billing.user_context", billingauth.UserContext{...})
//	c.Request = c.Request.WithContext(billingauth.SetUserContext(ctx, uc))
//
// Handlers then retrieve user context via ginauth.UserContextFromGin(c).
//
// Implementations:
//   - AuthKit-backed provider (internal/auth) - wraps JWT verifier for standalone mode
//   - Custom providers - implement this interface to use your own auth system
//
// Example custom implementation:
//
//	type MyAuthProvider struct { ... }
//	func (p *MyAuthProvider) Required() gin.HandlerFunc {
//	    return func(c *gin.Context) {
//	        // Validate auth, extract user info
//	        uc := billingauth.UserContext{UserID: "...", Email: "..."}
//	        c.Set("billing.user_context", uc)
//	        c.Request = c.Request.WithContext(billingauth.SetUserContext(c.Request.Context(), uc))
//	        c.Next()
//	    }
//	}
//	func (p *MyAuthProvider) Optional() gin.HandlerFunc { ... }
type Provider interface {
	// Required returns middleware that requires authentication.
	// Requests without valid auth will be rejected with 401.
	// Must set UserContext in Gin context on success.
	Required() gin.HandlerFunc

	// Optional returns middleware that attempts authentication but allows unauthenticated requests.
	// UserContext will be empty for unauthenticated requests.
	Optional() gin.HandlerFunc
}
