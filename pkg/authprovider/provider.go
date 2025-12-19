package authprovider

import (
	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/gin-gonic/gin"
)

// Provider is the app-facing auth boundary for verification middleware and typed claims access.
// Billing is a verifier-only service; it does not mount AuthKit routes or mint tokens.
type Provider interface {
	Required() gin.HandlerFunc
	Optional() gin.HandlerFunc
	Claims(c *gin.Context) (authgin.Claims, bool)
}
