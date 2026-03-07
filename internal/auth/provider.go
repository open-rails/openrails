package auth

import (
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	jwt "github.com/golang-jwt/jwt/v5"
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

		uc := userContextFromMap(raw)
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

		uc := userContextFromMap(raw)
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

func userContextFromMap(raw jwt.MapClaims) authprovider.UserContext {
	var uc authprovider.UserContext
	if raw == nil {
		return uc
	}

	if v, _ := raw["sub"].(string); v != "" {
		uc.UserID = v
	}
	if v, _ := raw["email"].(string); v != "" {
		uc.Email = v
	}
	if v, ok := raw["email_verified"].(bool); ok {
		uc.EmailVerified = v
	}
	if v, _ := raw["username"].(string); v != "" {
		uc.Username = v
	}
	if v, _ := raw["preferred_username"].(string); v != "" && uc.Username == "" {
		uc.Username = v
	}
	if v, _ := raw["discord_username"].(string); v != "" {
		uc.DiscordUsername = v
	}

	if v, _ := raw["sid"].(string); v != "" {
		uc.SessionID = v
	}

	uc.Roles = toStringSlice(raw["roles"])
	uc.Entitlements = toStringSlice(raw["entitlements"])
	return uc
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
