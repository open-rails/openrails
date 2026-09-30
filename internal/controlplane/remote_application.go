package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/internal/credential"
)

// ErrRemoteApplicationNotConfigured indicates the control plane has no verifier
// to validate a remote application access token. A wiring bug (#469): fail closed.
var ErrRemoteApplicationNotConfigured = errors.New("controlplane: remote_application verifier not configured")

// ErrNotRemoteApplicationToken indicates the credential is not a remote
// application access token (typ remote-application-access+jwt); the caller
// tries its other resolvers.
var ErrNotRemoteApplicationToken = credential.ErrNotRemoteApplicationToken

// LooksLikeJWT reports whether token has the three-segment compact-JWS shape, so
// the middleware can route a non-API-key bearer to JWT verification rather than
// rejecting it. It does NOT validate the token.
var LooksLikeJWT = credential.LooksLikeJWT

// remoteApplicationTokenType is the JOSE typ of a remote application acting
// as itself.
const remoteApplicationTokenType = "remote-application-access+jwt"

// ResolveRemoteApplication verifies a remote application's access token
// (#76/#484) and resolves it into the merchant-scoped service credential API
// keys and service JWTs produce, so the #481 role-based merchant authz runs
// unchanged. Its permissions are the application's stored grants, read live;
// a permissions claim may only narrow them. The merchant is the one its
// controlling group backs (#567).
//
// Errors: ErrNotRemoteApplicationToken for another kind of token,
// ErrDelegatedInvalid for a failed verification, ErrDelegatedIssuerUnknown
// when the controlling group backs no active merchant.
func (c *ControlPlane) ResolveRemoteApplication(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrRemoteApplicationNotConfigured
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrDelegatedInvalid
	}
	if !strings.EqualFold(joseType(token), remoteApplicationTokenType) {
		return nil, ErrNotRemoteApplicationToken
	}
	cl, err := c.delegatedVerifier.Verify(ctx, token)
	if err != nil || cl.Kind != iam.ActorRemoteApplication {
		return nil, ErrDelegatedInvalid
	}
	mid, slug, err := c.merchantForApplication(ctx, cl)
	if err != nil {
		return nil, err
	}
	return &ResolvedServiceCredential{
		OwnerGroupID:  cl.Group.GroupID,
		OwnerGroupRef: slug,
		MerchantID:    mid,
		MerchantSlug:  slug,
		Permissions:   cl.Permissions,
	}, nil
}

// joseType is a compact JWS's unverified typ header, "" when unreadable.
func joseType(token string) string {
	header, _, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return ""
	}
	var h struct {
		Typ string `json:"typ"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	return strings.TrimSpace(h.Typ)
}
