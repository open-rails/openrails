package auth

import (
	"errors"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	authhttp "github.com/open-rails/authkit/adapters/http"
	"github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/config"
)

// Verifier validates bearer tokens against configured issuers/JWKS.
type Verifier interface {
	Verify(token string) (jwt.MapClaims, error)
}

// BuildAcceptConfig returns an authkit AcceptConfig based on billing auth config.
// Supports multiple issuers to accept tokens from multiple IdPs/environments.
func BuildAcceptConfig(cfg *config.AuthConfig) (core.AcceptConfig, error) {
	if cfg == nil {
		return core.AcceptConfig{}, errors.New("auth config is required")
	}

	if len(cfg.Issuers) == 0 {
		return core.AcceptConfig{}, errors.New("at least one auth issuer is required")
	}

	expectedAudience := strings.TrimSpace(cfg.ExpectedAudience)

	// Build IssuerAccept config for each issuer
	issuerAccepts := make([]core.IssuerAccept, 0, len(cfg.Issuers))
	for _, issuer := range cfg.Issuers {
		issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
		if issuer == "" {
			continue
		}

		issCfg := core.IssuerAccept{Issuer: issuer}
		if expectedAudience != "" {
			issCfg.Audiences = []string{expectedAudience}
		}

		issuerAccepts = append(issuerAccepts, issCfg)
	}

	if len(issuerAccepts) == 0 {
		return core.AcceptConfig{}, errors.New("no valid issuers configured")
	}

	accept := core.AcceptConfig{
		Issuers:    issuerAccepts,
		Algorithms: []string{"RS256"},
		Skew:       60 * time.Second,
	}

	return accept, nil
}

// NewVerifier builds an authkit-backed verifier using billing auth config.
// Supports multiple issuers to accept tokens from multiple IdPs/environments.
func NewVerifier(cfg *config.AuthConfig) (Verifier, error) {
	accept, err := BuildAcceptConfig(cfg)
	if err != nil {
		return nil, err
	}
	return authhttp.NewVerifier(accept), nil
}

// FormatVerifierError normalises verifier error messages for HTTP responses.
func FormatVerifierError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "missing_token"):
		return "missing_token"
	case strings.Contains(msg, "bad_issuer"):
		return "invalid_issuer"
	case strings.Contains(msg, "bad_audience"):
		return "invalid_audience"
	case strings.Contains(msg, "invalid_token"):
		return "invalid_or_expired_token"
	default:
		return msg
	}
}
