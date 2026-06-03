package routes

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
)

// DelegatedMinter mints short-lived, user-scoped delegated access tokens for the
// browser tier (issue #222). The control plane implements it; tests inject a
// fake. nil => the deployment cannot mint (verifier-only mode) and the mint route
// is not mounted.
type DelegatedMinter interface {
	MintDelegatedAccessToken(ctx context.Context, p controlplane.MintDelegatedParams) (*controlplane.MintedDelegatedToken, error)
}

// mintDelegatedTokenRequest is the body of POST /v1/service/delegated-tokens.
// The tenant is deliberately ABSENT: it is resolved server-side from the calling
// OAT so a host backend can never mint cross-tenant.
type mintDelegatedTokenRequest struct {
	// DelegatedSub is the end-user id the minted browser token acts as.
	DelegatedSub string `json:"delegated_sub" binding:"required"`
	// Permissions are the requested `openrails:self:*` grants for the token.
	Permissions []string `json:"permissions" binding:"required"`
	// TTLSeconds is the requested lifetime in seconds. Optional; defaults to
	// controlplane.MintDefaultTTL and is capped at controlplane.MintMaxTTL.
	TTLSeconds int `json:"ttl_seconds"`
}

// mintDelegatedTokenResponse is the body returned to the host backend.
type mintDelegatedTokenResponse struct {
	// Token is the signed delegated access token (JWT) to hand to the browser.
	Token string `json:"token"`
	// ExpiresAt is the token's expiry as an RFC3339 timestamp.
	ExpiresAt string `json:"expires_at"`
}

// mintDelegatedTokenHandler builds the POST /v1/service/delegated-tokens handler.
//
// It is the MINT side of the browser-direct delegated-token tier: a tenant's host
// backend (authenticated by an OpenRails OAT holding PermSelfMint) asks OpenRails
// to mint a short-lived, user-scoped delegated access token it can hand to a
// browser for the `/v1/self/*` surface. Host backends do NOT hold the control
// plane's signing key, so OpenRails mints on their behalf.
//
// SECURITY: the minted token's tenant is taken from the CALLING OAT (resolved by
// OATRequired), NEVER from the request body. This makes cross-tenant minting
// impossible. Every requested permission must be in the `openrails:self:*` self
// catalog; any other permission is rejected.
func mintDelegatedTokenHandler(minter DelegatedMinter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if minter == nil {
			response.InternalError(c, "delegated mint not configured")
			c.Abort()
			return
		}

		// Resolve the tenant from the authenticated OAT — never the body.
		oat, ok := ginmw.OATFromGin(c)
		if !ok || oat == nil {
			response.UnauthorizedWithMessage(c, "oat required")
			c.Abort()
			return
		}

		var req mintDelegatedTokenRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid_request")
			c.Abort()
			return
		}

		var ttl time.Duration
		if req.TTLSeconds > 0 {
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}

		minted, err := minter.MintDelegatedAccessToken(c.Request.Context(), controlplane.MintDelegatedParams{
			// Tenant is bound to the OAT's owning org slug — the SAME value the
			// delegated verifier maps back to this tenant. Body cannot override it.
			Tenant:           oat.OrgSlug,
			DelegatedSubject: req.DelegatedSub,
			Permissions:      req.Permissions,
			TTL:              ttl,
		})
		if err != nil {
			var forbidden *controlplane.ErrMintForbiddenPermission
			switch {
			case errors.As(err, &forbidden):
				response.ForbiddenWithMessage(c, "permission_not_self_service")
			case errors.Is(err, controlplane.ErrMintNoSubject):
				response.BadRequest(c, "delegated_sub_required")
			case errors.Is(err, controlplane.ErrMintNoPermissions):
				response.BadRequest(c, "permissions_required")
			case errors.Is(err, controlplane.ErrOATTenantUnresolved):
				response.ForbiddenWithMessage(c, "oat_tenant_unresolved")
			case errors.Is(err, controlplane.ErrMintNotConfigured):
				response.InternalError(c, "delegated mint not configured")
			default:
				log.WithError(err).Warn("delegated token mint failed")
				response.InternalError(c, "delegated mint failed")
			}
			c.Abort()
			return
		}

		c.JSON(http.StatusOK, mintDelegatedTokenResponse{
			Token:     minted.Token,
			ExpiresAt: minted.ExpiresAt.Format(time.RFC3339),
		})
	}
}
