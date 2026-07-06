package handlers

// Merchant team management (#760): the roster, invites, role changes, and member
// removal behind /v1/merchant/team, all through AuthKit group membership via the
// control plane. Reads gate on merchant:members:read; mutations on
// merchant:members:manage — owner-only in the fixed #567 catalog (mirrors the
// #757 api-key surface). The last-owner invariant lives in the control plane and
// surfaces here as a corrective 400.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantTeamManager is the control-plane surface behind the team routes.
// Implemented by *controlplane.ControlPlane; nil (an embedded host without a
// control plane) leaves the routes answering 501.
type MerchantTeamManager interface {
	ListMerchantTeam(ctx context.Context, mid merchant.ID) ([]controlplane.MerchantTeamMember, error)
	InviteMerchantTeamMember(ctx context.Context, mid merchant.ID, email, role, actorUserID string) (controlplane.MerchantTeamInviteResult, error)
	ListMerchantTeamInvites(ctx context.Context, mid merchant.ID) ([]controlplane.MerchantTeamInvite, error)
	InvitesEnabled() bool
	RevokeMerchantTeamInvite(ctx context.Context, mid merchant.ID, linkID string) (bool, error)
	ChangeMerchantTeamRole(ctx context.Context, mid merchant.ID, targetUserID, newRole, actorUserID string) error
	RemoveMerchantTeamMember(ctx context.Context, mid merchant.ID, targetUserID, actorUserID string) error
}

func teamServiceUnavailable(r *httprequest.Request) {
	r.ErrorJSON(http.StatusNotImplemented, "team management requires the OpenRails control plane (not attached on this deployment)")
}

func teamMerchantScope(r *httprequest.Request) (merchant.ID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
		return merchant.ID{}, false
	}
	return mid, true
}

// MerchantListTeam handles GET /v1/merchant/team: the merchant's human team
// (user, role) — owner first — WITHOUT the synthetic bootstrap actor.
func MerchantListTeam(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		members, err := svc.ListMerchantTeam(r.Request.Context(), mid)
		if err != nil {
			teamServiceError(r, err, "failed to list team")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"data": members})
	}
}

// MerchantInviteTeamMember handles POST /v1/merchant/team/invites {email, role}.
// An existing user is added immediately (201 {added:true, member}); an
// unregistered email yields a single-use register+join link (201 {invite, url})
// when the deployment permits self-registration, else 409.
func MerchantInviteTeamMember(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		var req struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !r.BindJSON(&req) {
			return
		}
		req.Email = strings.TrimSpace(req.Email)
		req.Role = strings.ToLower(strings.TrimSpace(req.Role))
		if req.Email == "" {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_email",
				"email is required"))
			return
		}
		if _, known := controlplane.MerchantRolePermissions(req.Role); !known {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
				"role must be one of: "+strings.Join(controlplane.MerchantRoles(), ", ")))
			return
		}

		actor, ok := teamMutationActor(r, req.Role)
		if !ok {
			return
		}
		result, err := svc.InviteMerchantTeamMember(r.Request.Context(), mid, req.Email, req.Role, actor)
		if err != nil {
			switch {
			case errors.Is(err, controlplane.ErrTeamInvitesDisabled):
				r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "invites_disabled",
					"that email has no account yet, and self-registration invites are disabled on this deployment — the operator must provision the account first, then add it here by email"))
			case errors.Is(err, authkit.ErrExternalInvitesDisabled):
				r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "invites_disabled",
					"self-registration invites are disabled on this deployment"))
			default:
				teamMutationError(r, err, "failed to invite team member")
			}
			return
		}
		r.JSON(http.StatusCreated, result)
	}
}

// MerchantListTeamInvites handles GET /v1/merchant/team/invites: the merchant's
// invite links (pending/redeemed/revoked — never the code), plus whether the
// deployment can mint new-user links at all.
func MerchantListTeamInvites(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		invites, err := svc.ListMerchantTeamInvites(r.Request.Context(), mid)
		if err != nil {
			teamServiceError(r, err, "failed to list invites")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"data": invites, "invites_enabled": svc.InvitesEnabled()})
	}
}

// MerchantRevokeTeamInvite handles DELETE /v1/merchant/team/invites/{id}: revokes
// the invite link, scoped to the caller's merchant (cross-merchant ids 404).
func MerchantRevokeTeamInvite(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		id := strings.TrimSpace(r.Param("id"))
		revoked, err := svc.RevokeMerchantTeamInvite(r.Request.Context(), mid, id)
		if err != nil {
			teamServiceError(r, err, "failed to revoke invite")
			return
		}
		if !revoked {
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found",
				"no pending invite with that id in this merchant"))
			return
		}
		r.JSON(http.StatusOK, map[string]any{"revoked": true, "id": id})
	}
}

// MerchantChangeTeamRole handles PATCH /v1/merchant/team/{user_id} {role}: sets
// the member's role. Demoting the last owner is a corrective 400.
func MerchantChangeTeamRole(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		userID := strings.TrimSpace(r.Param("user_id"))
		if userID == "" {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_user",
				"user id is required"))
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if !r.BindJSON(&req) {
			return
		}
		req.Role = strings.ToLower(strings.TrimSpace(req.Role))
		if _, known := controlplane.MerchantRolePermissions(req.Role); !known {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
				"role must be one of: "+strings.Join(controlplane.MerchantRoles(), ", ")))
			return
		}
		actor, ok := teamMutationActor(r, req.Role)
		if !ok {
			return
		}
		if err := svc.ChangeMerchantTeamRole(r.Request.Context(), mid, userID, req.Role, actor); err != nil {
			teamMutationError(r, err, "failed to change role")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"user_id": userID, "role": req.Role})
	}
}

// MerchantRemoveTeamMember handles DELETE /v1/merchant/team/{user_id}: removes
// the member. Removing the last owner is a corrective 400.
func MerchantRemoveTeamMember(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		if svc == nil {
			teamServiceUnavailable(r)
			return
		}
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		userID := strings.TrimSpace(r.Param("user_id"))
		if userID == "" {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_user",
				"user id is required"))
			return
		}
		// Removal grants no role, so there is no target role to no-escalation-check
		// against a non-user principal; the members:manage route gate suffices.
		actor := teamActorNoEscalation(r)
		if err := svc.RemoveMerchantTeamMember(r.Request.Context(), mid, userID, actor); err != nil {
			teamMutationError(r, err, "failed to remove team member")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"removed": true, "user_id": userID})
	}
}

// teamMutationActor resolves the acting user for a role-granting mutation and
// enforces no-escalation for non-user principals (same shape as #757): a USER
// SESSION passes its user id to AuthKit core (which enforces authority against
// live state); a NON-USER credential carries its resolved grants and must cover
// the target role's permissions here before a genesis-actor mutation. Returns
// (actor, false) and writes a 403 when coverage fails.
func teamMutationActor(r *httprequest.Request, role string) (string, bool) {
	principal, hasPrincipal := merchantRoutePrincipal(r)
	if hasPrincipal && len(principal.Permissions) > 0 {
		if !controlplane.MerchantRoleCoveredBy(role, principal.Permissions) {
			r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
				"cannot grant a role with authority beyond your own credential's"))
			return "", false
		}
		return "", true // non-user principal → genesis actor in the control plane
	}
	uc, _ := r.UserContext()
	actor := strings.TrimSpace(uc.UserID)
	if actor == "" {
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "members_manage_required",
			"caller identity does not support managing team members"))
		return "", false
	}
	return actor, true
}

// teamActorNoEscalation resolves the acting user for a non-granting mutation
// (removal): the caller's user id when it is a user session, else empty (the
// control plane falls back to the genesis actor). Authority is already enforced
// by the members:manage route gate.
func teamActorNoEscalation(r *httprequest.Request) string {
	uc, _ := r.UserContext()
	return strings.TrimSpace(uc.UserID)
}

func teamServiceError(r *httprequest.Request, err error, fallback string) {
	if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
		r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
		return
	}
	r.ErrorJSON(http.StatusInternalServerError, fallback)
}

func teamMutationError(r *httprequest.Request, err error, fallback string) {
	switch {
	case errors.Is(err, controlplane.ErrCannotRemoveLastOwner):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "last_owner",
			"a merchant must keep at least one owner — assign another owner before demoting or removing this one"))
	case errors.Is(err, controlplane.ErrNotATeamMember):
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found",
			"that user is not a member of this merchant"))
	case errors.Is(err, controlplane.ErrUnknownMerchantRole):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
			"role must be one of: "+strings.Join(controlplane.MerchantRoles(), ", ")))
	case errors.Is(err, authkit.ErrInsufficientRoleAuthority):
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "members_manage_required",
			"your account lacks team-management authority on this merchant"))
	case errors.Is(err, authkit.ErrRoleAssignmentEscalation):
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
			"cannot grant a role with authority beyond your own"))
	default:
		teamServiceError(r, err, fallback)
	}
}
