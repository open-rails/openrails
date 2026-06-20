package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrRemoteApplicationNotConfigured indicates the control plane has no verifier
// to validate a JWKS-principal self-token. A wiring bug (#469): fail closed.
var ErrRemoteApplicationNotConfigured = errors.New("controlplane: remote_application verifier not configured")

// ErrNotRemoteApplicationToken indicates the presented credential verified but is
// not a JWKS-principal self-token (typ=remote-application-access+jwt). The caller
// must NOT treat it as a remote_application credential.
var ErrNotRemoteApplicationToken = errors.New("controlplane: not a remote_application self-token")

// LooksLikeJWT reports whether token has the three-segment compact-JWS shape, so
// the middleware can route a non-service-token bearer to JWT verification rather
// than rejecting it. It does NOT validate the token.
func LooksLikeJWT(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	return strings.Count(token, ".") == 2
}

// ResolveRemoteApplication validates a JWKS-principal SELF-token (#76/#484) and
// resolves the caller into a *ResolvedServiceToken — the SAME shape service-token
// auth produces — so the existing #481 role-based merchant authz runs unchanged.
//
// AuthKit's verifier resolves the principal's STORED authority from the VALIDATED
// `iss` (its registered remote_application -> assigned org roles + direct
// permissions); the token's own self-claimed permissions/roles are IGNORED. The
// merchant the caller administers is resolved by ROLE on the merchant's
// owner_org_id (#481), never by an identity-equation — exactly the
// service-token model.
//
// Returns:
//   - ErrNotRemoteApplicationToken when the token verifies but is not a
//     remote_application self-token (caller falls through / rejects),
//   - ErrDelegatedInvalid for any verification failure (sanitized),
//   - ErrServiceTokenMerchantUnresolved when the principal's org owns no
//     active merchant (fail closed).
func (c *ControlPlane) ResolveRemoteApplication(ctx context.Context, token string) (*ResolvedServiceToken, error) {
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

	// #481: authorize by the principal's role on a merchant's owner_org_id. The
	// principal's STORED org memberships (Claims.Org slug) name the owning
	// AuthKit org; resolve the merchant it owns. Authority is the resolved
	// role/perms, never the token's self-claims.
	mid, mslug, ownerOrgID, err := c.merchantForRemoteApplication(ctx, cl)
	if err != nil {
		return nil, err
	}

	return &ResolvedServiceToken{
		OwnerOrgID:   ownerOrgID,
		OwnerOrgSlug: strings.TrimSpace(cl.Org),
		MerchantID:   mid,
		MerchantSlug: mslug,
		Permissions:  append([]string(nil), cl.Permissions...),
		// A JWKS principal carries merchant-WIDE authority over the merchant its
		// owning org owns; scope it to that merchant resource so subject-scope
		// checks (AllowsCustomer) behave like a merchant-wide service token.
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

// merchantForRemoteApplication resolves the OpenRails merchant a JWKS principal
// administers (#481), by ROLE on the owning AuthKit org: the principal's
// STORED org memberships -> the AuthKit org uuid -> the merchant whose
// owner_org_id is that uuid. Returns the merchant id/slug and the resolved
// owner_org_id (the caller's authority anchor). Fails closed when no
// membership owns an active merchant.
func (c *ControlPlane) merchantForRemoteApplication(ctx context.Context, cl authhttp.Claims) (merchant.ID, string, string, error) {
	if c == nil || c.Core() == nil || c.pool == nil {
		return merchant.ID{}, "", "", errors.New("controlplane: control plane unavailable for remote_application resolution")
	}

	// The principal's assigned org slug(s). Claims.Org is the first
	// membership; AuthKit surfaces a single membership as Org. Resolve its
	// AuthKit uuid, then the merchant owned by it.
	slug := strings.TrimSpace(cl.Org)
	if slug == "" {
		return merchant.ID{}, "", "", ErrServiceTokenMerchantUnresolved
	}
	t, err := c.Core().ResolveOrgBySlug(ctx, slug)
	if err != nil || t == nil {
		return merchant.ID{}, "", "", ErrServiceTokenMerchantUnresolved
	}
	mid, mslug, err := c.merchantDirectoryRow(ctx, `owner_org_id = $1`, t.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", "", ErrServiceTokenMerchantUnresolved
	}
	if err != nil {
		return merchant.ID{}, "", "", err
	}
	return mid, mslug, t.ID, nil
}
