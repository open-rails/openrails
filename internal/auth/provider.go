package auth

import (
	"strings"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/pkg/authprovider"
	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	jwt "github.com/golang-jwt/jwt/v5"
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

func (p *authKitProvider) Claims(c *gin.Context) (authgin.Claims, bool) {
	return authgin.ClaimsFromGin(c)
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

		cl := claimsFromMap(raw)
		c.Set("authkit.claims", cl)
		c.Request = c.Request.WithContext(authgin.SetClaims(c.Request.Context(), cl))
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

		cl := claimsFromMap(raw)
		c.Set("authkit.claims", cl)
		c.Request = c.Request.WithContext(authgin.SetClaims(c.Request.Context(), cl))
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

func claimsFromMap(raw jwt.MapClaims) authgin.Claims {
	var cl authgin.Claims
	if raw == nil {
		return cl
	}

	if v, _ := raw["sub"].(string); v != "" {
		cl.UserID = v
	}
	if v, _ := raw["email"].(string); v != "" {
		cl.Email = v
	}
	if v, ok := raw["email_verified"].(bool); ok {
		cl.EmailVerified = v
	}
	if v, _ := raw["username"].(string); v != "" {
		cl.Username = v
	}
	if v, _ := raw["preferred_username"].(string); v != "" && cl.Username == "" {
		cl.Username = v
	}
	if v, _ := raw["discord_username"].(string); v != "" {
		cl.DiscordUsername = v
	}

	// Session ID: accept both sid (authkit) and session_id (legacy)
	if v, _ := raw["sid"].(string); v != "" {
		cl.SessionID = v
	} else if v, _ := raw["session_id"].(string); v != "" {
		cl.SessionID = v
	}

	cl.Roles = toStringSlice(raw["roles"])
	cl.Entitlements = toStringSlice(raw["entitlements"])
	return cl
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, el := range t {
			if s, ok := el.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
