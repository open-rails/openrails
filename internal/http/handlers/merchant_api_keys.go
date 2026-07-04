package handlers

// Merchant self-serve API keys (#757): mint/list/revoke scoped credentials for
// agents and integrations, all through AuthKit core via the control plane.
// The routes are gated on merchant:credentials:manage (owner-only in the fixed
// #567 catalog). The secret is returned EXACTLY ONCE, in the mint response.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantAPIKeyManager is the control-plane surface behind the self-serve
// API-key routes. Implemented by *controlplane.ControlPlane; nil (an embedded
// host without a control plane) leaves the routes answering 501.
type MerchantAPIKeyManager interface {
	MintMerchantAPIKey(ctx context.Context, mid merchant.ID, name, role, actorUserID string) (controlplane.MerchantAPIKey, string, error)
	ListMerchantAPIKeys(ctx context.Context, mid merchant.ID) ([]controlplane.MerchantAPIKey, error)
	RevokeMerchantAPIKey(ctx context.Context, mid merchant.ID, id string) (bool, error)
}

// merchantRoutePrincipal returns the gate-resolved principal the merchant
// permission middleware pinned onto the request.
func merchantRoutePrincipal(r *httprequest.Request) (billingauth.Principal, bool) {
	v, ok := r.Get(MerchantRoutePrincipalContextKey)
	if !ok {
		return billingauth.Principal{}, false
	}
	p, ok := v.(billingauth.Principal)
	return p, ok
}

func apiKeyMerchantScope(r *httprequest.Request) (merchant.ID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
		return merchant.ID{}, false
	}
	return mid, true
}

func apiKeyServiceUnavailable(r *httprequest.Request) {
	r.ErrorJSON(http.StatusNotImplemented, "API-key management requires the OpenRails control plane (not attached on this deployment)")
}

// MerchantCreateAPIKey handles POST /v1/merchant/api-keys {name, role} → 201
// {id, name, role, prefix, created_at, secret}. The secret is shown exactly
// once — it is never stored and never retrievable again. role must be one of
// the fixed merchant catalog roles (#567): viewer (read-only — the right
// choice for LLM agents), support, or owner.
func MerchantCreateAPIKey(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			apiKeyServiceUnavailable(r)
			return
		}
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		var req struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if !r.BindJSON(&req) {
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.Role = strings.ToLower(strings.TrimSpace(req.Role))
		if req.Name == "" || len(req.Name) > 120 {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_name",
				"name is required (max 120 chars)"))
			return
		}
		if _, known := controlplane.MerchantRolePermissions(req.Role); !known {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
				"role must be one of: "+strings.Join(controlplane.MerchantAPIKeyRoles(), ", ")))
			return
		}

		// Two actor shapes (mirroring the gate): a USER SESSION carries an AuthKit
		// user id and no resolved grant set — pass it as the mint actor so AuthKit
		// core enforces capability + no-escalation against live group state. Every
		// NON-USER credential (API key, service JWT, in-process host, delegated
		// token) carries its resolved grants on the principal instead — the same
		// no-escalation rule is enforced HERE (the requested role's permissions
		// must be covered by the caller's own) before a genesis-actor mint.
		principal, hasPrincipal := merchantRoutePrincipal(r)
		var actor string
		if hasPrincipal && len(principal.Permissions) > 0 {
			if !controlplane.MerchantRoleCoveredBy(req.Role, principal.Permissions) {
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
					"cannot mint a key with authority beyond your own credential's"))
				return
			}
		} else {
			uc, _ := r.UserContext()
			actor = strings.TrimSpace(uc.UserID)
			if actor == "" {
				// Fail closed: no user actor and no resolved grant set.
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "credentials_manage_required",
					"caller identity does not support minting API keys"))
				return
			}
		}

		key, secret, err := svc.MintMerchantAPIKey(r.Request.Context(), mid, req.Name, req.Role, actor)
		if err != nil {
			switch {
			case errors.Is(err, controlplane.ErrUnknownMerchantRole):
				r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
					"role must be one of: "+strings.Join(controlplane.MerchantAPIKeyRoles(), ", ")))
			case errors.Is(err, authkit.ErrInsufficientRoleAuthority):
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "credentials_manage_required",
					"your account lacks credential-management authority on this merchant"))
			case errors.Is(err, authkit.ErrRoleAssignmentEscalation):
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
					"cannot mint a key with authority beyond your own"))
			case errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved):
				r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
			default:
				r.ErrorJSON(http.StatusInternalServerError, "failed to mint API key")
			}
			return
		}
		r.JSON(http.StatusCreated, struct {
			controlplane.MerchantAPIKey
			Secret string `json:"secret"`
		}{MerchantAPIKey: key, Secret: secret})
	}
}

// MerchantListAPIKeys handles GET /v1/merchant/api-keys: every key of the
// caller's merchant (live, expired, revoked — status is the audit view),
// WITHOUT secret material.
func MerchantListAPIKeys(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			apiKeyServiceUnavailable(r)
			return
		}
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		keys, err := svc.ListMerchantAPIKeys(r.Request.Context(), mid)
		if err != nil {
			if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
				r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
				return
			}
			r.ErrorJSON(http.StatusInternalServerError, "failed to list API keys")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"data": keys})
	}
}

// MerchantRevokeAPIKey handles DELETE /v1/merchant/api-keys/{id}: revokes the
// key, scoped to the caller's merchant (cross-merchant ids 404).
func MerchantRevokeAPIKey(svc MerchantAPIKeyManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			apiKeyServiceUnavailable(r)
			return
		}
		mid, ok := apiKeyMerchantScope(r)
		if !ok {
			return
		}
		id := strings.TrimSpace(r.Param("id"))
		if _, err := uuid.Parse(id); err != nil {
			// Key ids are UUIDs; a malformed id can't match anything (and would
			// otherwise error inside the ::uuid cast).
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found",
				"no live API key with that id in this merchant"))
			return
		}
		revoked, err := svc.RevokeMerchantAPIKey(r.Request.Context(), mid, id)
		if err != nil {
			if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
				r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
				return
			}
			r.ErrorJSON(http.StatusInternalServerError, "failed to revoke API key")
			return
		}
		if !revoked {
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found",
				"no live API key with that id in this merchant"))
			return
		}
		r.JSON(http.StatusOK, map[string]any{"revoked": true, "id": id})
	}
}
