package standalonehandlers

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

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
)

// MerchantTeamManager is the control-plane surface behind the team routes.
// Implemented by *controlplane.ControlPlane; nil (an embedded host without a
// control plane) omits these routes at registration.
type MerchantTeamManager interface {
	RequestActor
	ListMerchantTeam(ctx context.Context, mid billing.MerchantID) ([]controlplane.MerchantTeamMember, error)
	InviteMerchantTeamMember(ctx context.Context, mid billing.MerchantID, email string, role iam.Role, actor iam.Actor) (controlplane.MerchantTeamInviteResult, error)
	ListMerchantTeamInvites(ctx context.Context, mid billing.MerchantID) ([]controlplane.MerchantTeamInvite, error)
	InvitesEnabled() bool
	RevokeMerchantTeamInvite(ctx context.Context, mid billing.MerchantID, id string, actor iam.Actor) (bool, error)
	ChangeMerchantTeamRole(ctx context.Context, mid billing.MerchantID, targetUserID string, newRole iam.Role, actor iam.Actor) error
	RemoveMerchantTeamMember(ctx context.Context, mid billing.MerchantID, targetUserID string, actor iam.Actor) error
}

func teamMerchantScope(r *httprequest.Request) (billing.MerchantID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeMerchantUnresolved, "")
		return billing.MerchantID{}, false
	}
	return mid, true
}

// MerchantListTeam handles GET /v1/merchant/team: the merchant's team (user,
// role), owners first.
func MerchantListTeam(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
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
// A live account that verified the email is added immediately (201
// {added:true, member}); any other email yields a single-use register+join link
// (201 {invite, url}) when the deployment permits self-registration, else 409.
func MerchantInviteTeamMember(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
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
		if req.Email == "" {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_email",
				"email is required"))
			return
		}
		role, ok := merchantRole(r, req.Role, controlplane.MerchantRoles())
		if !ok {
			return
		}
		actor, ok := mutationActor(r, svc, &role, "members_manage_required")
		if !ok {
			return
		}
		result, err := svc.InviteMerchantTeamMember(r.Request.Context(), mid, req.Email, role, actor)
		if err != nil {
			switch {
			case errors.Is(err, controlplane.ErrTeamInvitesDisabled):
				r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "invites_disabled",
					"that email has no verified account, and self-registration invites are disabled on this deployment — the operator must provision the account and verify its email first, then add it here by email"))
			case errors.Is(err, iam.ErrExternalInvitesDisabled):
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
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		id := strings.TrimSpace(r.Param("id"))
		actor, ok := mutationActor(r, svc, nil, "members_manage_required")
		if !ok {
			return
		}
		revoked, err := svc.RevokeMerchantTeamInvite(r.Request.Context(), mid, id, actor)
		if err != nil {
			teamMutationError(r, err, "failed to revoke invite")
			return
		}
		if !revoked {
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound,
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
		role, ok := merchantRole(r, req.Role, controlplane.MerchantRoles())
		if !ok {
			return
		}
		actor, ok := mutationActor(r, svc, &role, "members_manage_required")
		if !ok {
			return
		}
		if err := svc.ChangeMerchantTeamRole(r.Request.Context(), mid, userID, role, actor); err != nil {
			teamMutationError(r, err, "failed to change role")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"user_id": userID, "role": role.Name()})
	}
}

// MerchantRemoveTeamMember handles DELETE /v1/merchant/team/{user_id}: removes
// the member. Removing the last owner is a corrective 400.
func MerchantRemoveTeamMember(svc MerchantTeamManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
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
		// Removal grants no role: the members:manage route gate suffices for a
		// non-user principal.
		actor, ok := mutationActor(r, svc, nil, "members_manage_required")
		if !ok {
			return
		}
		if err := svc.RemoveMerchantTeamMember(r.Request.Context(), mid, userID, actor); err != nil {
			teamMutationError(r, err, "failed to remove team member")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"removed": true, "user_id": userID})
	}
}

func teamServiceError(r *httprequest.Request, err error, fallback string) {
	if errors.Is(err, controlplane.ErrServiceCredentialMerchantUnresolved) {
		r.ErrorCode(billing.CodeMerchantUnresolved, "")
		return
	}
	r.ErrorJSON(http.StatusInternalServerError, fallback)
}

func teamMutationError(r *httprequest.Request, err error, fallback string) {
	switch {
	case sessionRevoked(r, err):
	case errors.Is(err, controlplane.ErrCannotRemoveLastOwner):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "last_owner",
			"a merchant must keep at least one owner — assign another owner before demoting or removing this one"))
	case errors.Is(err, controlplane.ErrNotATeamMember):
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound,
			"that user is not a member of this merchant"))
	case errors.Is(err, controlplane.ErrUnknownMerchantRole):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "unknown_role",
			"role must be one of: "+strings.Join(controlplane.RoleNames(controlplane.MerchantRoles()), ", ")))
	case errors.Is(err, iam.ErrInsufficientAuthority):
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "members_manage_required",
			"your account lacks team-management authority on this merchant"))
	case errors.Is(err, iam.ErrRoleAssignmentEscalation):
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation",
			"cannot grant a role with authority beyond your own"))
	default:
		teamServiceError(r, err, fallback)
	}
}
