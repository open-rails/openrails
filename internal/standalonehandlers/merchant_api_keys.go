package standalonehandlers

// Merchant self-serve API keys (#757): mint/list/revoke scoped credentials for
// agents and integrations, all through AuthKit core via the control plane.
// The routes are gated on merchant:credentials:manage (owner-only in the fixed
// #567 catalog). The secret is returned EXACTLY ONCE, in the mint response.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/http/handlers"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
)

// MerchantAPIKeyManager is the control-plane surface behind the self-serve
// API-key routes. Implemented by *controlplane.ControlPlane; nil (an embedded
// host without a control plane) omits these routes at registration.
type MerchantAPIKeyManager interface {
	RequestActor
	MintMerchantAPIKey(ctx context.Context, mid billing.MerchantID, name string, role iam.Role, actor iam.Actor) (controlplane.MerchantAPIKey, string, error)
	ListMerchantAPIKeys(ctx context.Context, mid billing.MerchantID) ([]controlplane.MerchantAPIKey, error)
	RevokeMerchantAPIKey(ctx context.Context, mid billing.MerchantID, id string, actor iam.Actor) (bool, error)
}

// RequestActor derives who performs a merchant mutation: the AuthKit actor of
// a request's user token (verify.ActorFromClaims), bound to its session, or,
// for a non-user credential, whether its grants cover a role.
type RequestActor interface {
	RequestActor(r *http.Request) (iam.Actor, error)
	RoleCoveredBy(role iam.Role, grants []string) (bool, error)
}

// mutationActor is who performs a merchant credential or membership mutation.
// A user session acts as itself: AuthKit checks its authority and session
// live. A non-user credential (API key, service JWT, in-process host,
// delegated token) carries its resolved grants, which must cover role here
// (when set); it then acts as the system. It writes a 403 with code when
// neither applies or coverage fails.
func mutationActor(r *httprequest.Request, svc RequestActor, role *iam.Role, code string) (iam.Actor, bool) {
	if principal, ok := merchantRoutePrincipal(r); ok && len(principal.Permissions) > 0 {
		if role != nil {
			covered, err := svc.RoleCoveredBy(*role, principal.Permissions)
			if err != nil {
				r.InternalError("resolve role permissions", err)
				return iam.Actor{}, false
			}
			if !covered {
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
					"cannot grant authority beyond your own credential's"))
				return iam.Actor{}, false
			}
		}
		return iam.SystemActor(), true
	}
	actor, err := svc.RequestActor(r.Request)
	if err != nil {
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, code,
			"caller identity does not support this operation"))
		return iam.Actor{}, false
	}
	return actor, true
}

// merchantRole reads a merchant role name from a request, writing a 400 when
// it is unknown.
func merchantRole(r *httprequest.Request, name string, allowed []iam.Role) (iam.Role, bool) {
	role, ok := controlplane.MerchantRole(name)
	if !ok || !slices.Contains(allowed, role) {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
			"role must be one of: "+strings.Join(controlplane.RoleNames(allowed), ", ")))
		return iam.Role{}, false
	}
	return role, true
}

// sessionRevoked answers a mutation AuthKit refused because the caller's
// session was revoked since its token was minted.
func sessionRevoked(r *httprequest.Request, err error) bool {
	if !errors.Is(err, iam.ErrSessionRevoked) {
		return false
	}
	r.APIError(api.NewAPIError(http.StatusUnauthorized, api.ErrorTypeAuthentication, "credential_revoked",
		"the session was revoked"))
	return true
}

// merchantRoutePrincipal returns the gate-resolved principal the merchant
// permission middleware pinned onto the request.
func merchantRoutePrincipal(r *httprequest.Request) (billingauth.Principal, bool) {
	v, ok := r.Get(handlers.MerchantRoutePrincipalContextKey)
	if !ok {
		return billingauth.Principal{}, false
	}
	p, ok := v.(billingauth.Principal)
	return p, ok
}

func apiKeyMerchantScope(r *httprequest.Request) (billing.MerchantID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeMerchantUnresolved, "")
		return billing.MerchantID{}, false
	}
	return mid, true
}

// MerchantCreateAPIKey handles POST /v1/merchant/api-keys {name, role} → 201
// {id, name, role, prefix, created_at, secret}. The secret is shown exactly
// once — it is never stored and never retrievable again. role must be one of
// the fixed merchant catalog roles (#567): viewer (read-only — the right
// choice for LLM agents), support, or owner.
func MerchantCreateAPIKey(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		var req billing.CreateAPIKeyRequest
		if !r.BindJSON(&req) {
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || len(req.Name) > 120 {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_name",
				"name is required (max 120 chars)"))
			return
		}
		role, ok := merchantRole(r, req.Role, controlplane.MerchantAPIKeyRoles())
		if !ok {
			return
		}
		actor, ok := mutationActor(r, svc, &role, "credentials_manage_required")
		if !ok {
			return
		}
		key, secret, err := svc.MintMerchantAPIKey(r.Request.Context(), mid, req.Name, role, actor)
		if err != nil {
			switch {
			case sessionRevoked(r, err):
			case errors.Is(err, iam.ErrInsufficientAuthority):
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "credentials_manage_required",
					"your account lacks credential-management authority on this merchant"))
			case errors.Is(err, iam.ErrRoleAssignmentEscalation):
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
					"cannot mint a key with authority beyond your own"))
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
				r.ErrorCode(billing.CodeMerchantUnresolved, "")
			default:
				r.ErrorJSON(http.StatusInternalServerError, "failed to mint API key")
			}
			return
		}
		r.JSON(http.StatusCreated, billing.CreatedAPIKey{APIKey: key, Secret: secret})
	}
}

// MerchantListAPIKeys handles GET /v1/merchant/api-keys: every key of the
// caller's merchant (live, expired, revoked — status is the audit view),
// WITHOUT secret material.
func MerchantListAPIKeys(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		keys, err := svc.ListMerchantAPIKeys(r.Request.Context(), mid)
		if err != nil {
			if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
				r.ErrorCode(billing.CodeMerchantUnresolved, "")
				return
			}
			r.ErrorJSON(http.StatusInternalServerError, "failed to list API keys")
			return
		}
		r.SuccessJSON(billing.ListPage[billing.APIKey]{Items: keys})
	}
}

// MerchantRevokeAPIKey handles DELETE /v1/merchant/api-keys/{id}: revokes the
// key, scoped to the caller's merchant (cross-merchant ids 404).
func MerchantRevokeAPIKey(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		id := strings.TrimSpace(r.Param("id"))
		if _, err := uuid.Parse(id); err != nil {
			// Key ids are UUIDs; a malformed id can't match anything (and would
			// otherwise error inside the ::uuid cast).
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound,
				"no live API key with that id in this merchant"))
			return
		}
		actor, ok := mutationActor(r, svc, nil, "credentials_manage_required")
		if !ok {
			return
		}
		revoked, err := svc.RevokeMerchantAPIKey(r.Request.Context(), mid, id, actor)
		if err != nil {
			if sessionRevoked(r, err) {
				return
			}
			if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
				r.ErrorCode(billing.CodeMerchantUnresolved, "")
				return
			}
			r.ErrorJSON(http.StatusInternalServerError, "failed to revoke API key")
			return
		}
		if !revoked {
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound,
				"no API key with that id in this merchant"))
			return
		}
		r.NoContent()
	}
}
