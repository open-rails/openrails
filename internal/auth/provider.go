package auth

import (
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	authhttp "github.com/open-rails/authkit/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
	log "github.com/sirupsen/logrus"
)

type authKitProvider struct {
	verifier Verifier
}

func NewProvider(cfg *config.AuthConfig) (authprovider.Provider, error) {
	v, err := NewVerifier(cfg)
	if err != nil {
		return nil, err
	}
	return &authKitProvider{verifier: v}, nil
}

func (p *authKitProvider) Required() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.verifier == nil {
			log.Warn("auth middleware misconfigured: no verifier provided")
			response.InternalError(c, "authentication disabled")
			c.Abort()
			return
		}

		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			response.UnauthorizedWithMessage(c, "authorization header required")
			c.Abort()
			return
		}

		raw, err := p.verifier.Verify(token)
		if err != nil {
			log.WithError(err).Warn("jwt verification failed")
			response.UnauthorizedWithMessage(c, FormatVerifierError(err))
			c.Abort()
			return
		}

		uc := userContextFromClaims(raw)
		c.Set("billing.user_context", uc)
		c.Request = c.Request.WithContext(authprovider.SetUserContext(c.Request.Context(), uc))
		c.Next()
	}
}

func (p *authKitProvider) Optional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.verifier == nil {
			c.Next()
			return
		}

		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.Next()
			return
		}

		raw, err := p.verifier.Verify(token)
		if err != nil {
			log.WithError(err).Debug("optional jwt verification failed")
			c.Next()
			return
		}

		uc := userContextFromClaims(raw)
		c.Set("billing.user_context", uc)
		c.Request = c.Request.WithContext(authprovider.SetUserContext(c.Request.Context(), uc))
		c.Next()
	}
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(header), strings.ToLower(prefix)) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func userContextFromClaims(cl authhttp.Claims) authprovider.UserContext {
	return authprovider.UserContext{
		UserID:          cl.UserID,
		Email:           cl.Email,
		EmailVerified:   cl.EmailVerified,
		Username:        cl.Username,
		DiscordUsername: cl.DiscordUsername,
		SessionID:       cl.SessionID,
		Roles:           cl.Roles,
		Entitlements:    cl.Entitlements,
		// Operator-org admin authority (#224, hardcut): the org slug + org-scoped
		// roles carried in the `org` / `org_roles` claims are the ONLY admin
		// authority path now that the legacy global-admin DB fallback is removed.
		Org:      cl.Org,
		OrgRoles: cl.OrgRoles,
	}
}
