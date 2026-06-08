package ginroutes

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/pkg/tenant"
)

// DelegatedIssuerAdmin manages a tenant's FEDERATED delegated-token issuers
// (issue #259). The control plane implements it; tests inject a fake. nil => the
// deployment cannot manage issuers (verifier-only mode) and the routes are not
// mounted.
type DelegatedIssuerAdmin interface {
	RegisterDelegatedIssuer(ctx context.Context, p controlplane.RegisterDelegatedIssuerParams) error
	DisableDelegatedIssuer(ctx context.Context, tenantID tenant.ID, issuer string) error
	ListDelegatedIssuers(ctx context.Context, tenantID tenant.ID) ([]controlplane.DelegatedIssuer, error)
}

// registerIssuerRequest is the body of POST /v1/service/tenant/issuers. The
// tenant is deliberately ABSENT: it is bound from the calling service token so a host
// backend can only register issuers under its OWN tenant.
type registerIssuerRequest struct {
	Issuer    string   `json:"issuer" binding:"required"`
	JWKSURI   string   `json:"jwks_uri" binding:"required"`
	Audiences []string `json:"audiences" binding:"required"`
}

type disableIssuerRequest struct {
	Issuer string `json:"issuer" binding:"required"`
}

type issuerView struct {
	Issuer    string   `json:"issuer"`
	JWKSURI   string   `json:"jwks_uri"`
	Audiences []string `json:"audiences"`
	Enabled   bool     `json:"enabled"`
}

// registerDelegatedIssuerHandler builds POST /v1/service/tenant/issuers — the
// service token-gated bootstrap/rotate route. A tenant's host backend (authenticated by an
// service token holding PermAdmin for its tenant) registers/rotates the issuer + JWKS URL
// it signs aud=openrails browser tokens with. The tenant is taken from the service token,
// never the body, so cross-tenant registration is impossible; global issuer
// uniqueness is enforced in the registry.
func registerDelegatedIssuerHandler(admin DelegatedIssuerAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceToken, ok := ginmw.ServiceTokenFromGin(c)
		if !ok || serviceToken == nil {
			response.UnauthorizedWithMessage(c, "service token required")
			c.Abort()
			return
		}
		var req registerIssuerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid_request")
			c.Abort()
			return
		}
		err := admin.RegisterDelegatedIssuer(c.Request.Context(), controlplane.RegisterDelegatedIssuerParams{
			TenantID:  serviceToken.TenantID,
			Issuer:    req.Issuer,
			JWKSURI:   req.JWKSURI,
			Audiences: req.Audiences,
		})
		if err != nil {
			writeIssuerError(c, err)
			return
		}
		c.JSON(http.StatusOK, issuerView{Issuer: req.Issuer, JWKSURI: req.JWKSURI, Audiences: req.Audiences, Enabled: true})
	}
}

// disableDelegatedIssuerHandler builds POST /v1/service/tenant/issuers/disable —
// the per-issuer kill-switch. Disables a single issuer owned by the caller's
// tenant without affecting the tenant's other issuers.
func disableDelegatedIssuerHandler(admin DelegatedIssuerAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceToken, ok := ginmw.ServiceTokenFromGin(c)
		if !ok || serviceToken == nil {
			response.UnauthorizedWithMessage(c, "service token required")
			c.Abort()
			return
		}
		var req disableIssuerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid_request")
			c.Abort()
			return
		}
		if err := admin.DisableDelegatedIssuer(c.Request.Context(), serviceToken.TenantID, req.Issuer); err != nil {
			writeIssuerError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"issuer": req.Issuer, "enabled": false})
	}
}

// listDelegatedIssuersHandler builds GET /v1/service/tenant/issuers — lists the
// caller tenant's registered issuers (enabled + disabled).
func listDelegatedIssuersHandler(admin DelegatedIssuerAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceToken, ok := ginmw.ServiceTokenFromGin(c)
		if !ok || serviceToken == nil {
			response.UnauthorizedWithMessage(c, "service token required")
			c.Abort()
			return
		}
		issuers, err := admin.ListDelegatedIssuers(c.Request.Context(), serviceToken.TenantID)
		if err != nil {
			writeIssuerError(c, err)
			return
		}
		out := make([]issuerView, 0, len(issuers))
		for _, i := range issuers {
			out = append(out, issuerView{Issuer: i.Issuer, JWKSURI: i.JWKSURI, Audiences: i.Audiences, Enabled: i.Enabled})
		}
		c.JSON(http.StatusOK, gin.H{"issuers": out})
	}
}

func writeIssuerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, controlplane.ErrIssuerOwnedByOtherTenant):
		response.ForbiddenWithMessage(c, "issuer_owned_by_other_tenant")
	case errors.Is(err, controlplane.ErrIssuerInvalidJWKSURI):
		response.BadRequest(c, "invalid_jwks_uri")
	case errors.Is(err, controlplane.ErrIssuerInvalidIssuer):
		response.BadRequest(c, "invalid_issuer")
	case errors.Is(err, controlplane.ErrIssuerAudiencesRequired):
		response.BadRequest(c, "audiences_required")
	case errors.Is(err, controlplane.ErrIssuerNotFound):
		response.NotFoundWithMessage(c, "issuer_not_found")
	case errors.Is(err, controlplane.ErrServiceTokenTenantUnresolved):
		response.ForbiddenWithMessage(c, "service_token_tenant_unresolved")
	default:
		log.WithError(err).Warn("delegated issuer management failed")
		response.InternalError(c, "issuer_management_failed")
	}
	c.Abort()
}
