package controlplane

import (
	"context"
	"errors"
	"strings"

	authcore "github.com/open-rails/authkit/core"
)

// ErrRemoteApplicationNotConfigured indicates the control plane has no verifier
// to validate a remote application access token. A wiring bug (#469): fail closed.
var ErrRemoteApplicationNotConfigured = errors.New("controlplane: remote_application verifier not configured")

// ErrNotRemoteApplicationToken indicates the presented credential verified but
// is not a remote application access token
// (typ=remote-application-access+jwt). The caller must NOT treat it as a
// remote_application credential.
var ErrNotRemoteApplicationToken = errors.New("controlplane: not a remote application access token")

// LooksLikeJWT reports whether token has the three-segment compact-JWS shape, so
// the middleware can route a non-API-key bearer to JWT verification rather than
// rejecting it. It does NOT validate the token.
func LooksLikeJWT(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	return strings.Count(token, ".") == 2
}

// ResolveRemoteApplication validates a remote application access token
// (#76/#484) and resolves the caller into the same merchant-scoped service-credential result
// used by API keys and service JWTs, so the existing #481 role-based merchant
// authz runs unchanged.
//
// AuthKit's verifier resolves the principal's STORED authority from the VALIDATED
// `iss` (its registered remote_application -> assigned org roles + direct
// permissions); the token's own self-claimed permissions/roles are IGNORED. The
// merchant the caller administers is resolved by ROLE on the merchant's
// permission_group_id (#481/#567), never by an identity-equation.
//
// Returns:
//   - ErrNotRemoteApplicationToken when the token verifies but is not a
//     remote application access token (caller falls through / rejects),
//   - ErrDelegatedInvalid for any verification failure (sanitized),
//   - ErrServiceCredentialMerchantUnresolved when the principal's org owns no
//     active merchant (fail closed).
func (c *ControlPlane) ResolveRemoteApplication(ctx context.Context, token string) (*ResolvedServiceCredential, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrRemoteApplicationNotConfigured
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrDelegatedInvalid
	}

	// Verify signature/issuer/audience/expiry. Verify() handles the
	// remote-application-access+jwt profile and resolves STORED authority from the
	// validated `iss`; self-claimed authority is never honored.
	cl, err := c.delegatedVerifier.Verify(token)
	if err != nil {
		if isRemoteApplicationWrongType(err) {
			return nil, ErrNotRemoteApplicationToken
		}
		return nil, ErrDelegatedInvalid
	}
	if !cl.IsRemoteApplication() {
		return nil, ErrNotRemoteApplicationToken
	}

	// #567: authorize by the remote_application's controlling permission-group.
	// The validated `iss` -> remote_application -> its merchant group resolves the
	// merchant; authority is the principal's STORED group-role perms (Permissions),
	// never the token's self-claims.
	mid, mslug, groupID, groupRef, _, err := c.merchantForIssuer(ctx, cl.Issuer)
	if err != nil {
		return nil, err
	}

	return &ResolvedServiceCredential{
		OwnerGroupID:  groupID,
		OwnerGroupRef: groupRef,
		MerchantID:    mid,
		MerchantSlug:  mslug,
		Permissions:   append([]string(nil), cl.Permissions...),
		// A remote application carries merchant-wide authority over the merchant
		// its controlling group owns; scope it to that merchant resource so
		// subject-scope checks (AllowsCustomer) behave like a merchant-wide credential.
		Resources: []authcore.APIKeyResource{MerchantResource(mid)},
	}, nil
}

func isRemoteApplicationWrongType(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.TrimSpace(err.Error())
	return msg == "access_token_wrong_typ" || msg == "delegated_access_wrong_typ"
}
