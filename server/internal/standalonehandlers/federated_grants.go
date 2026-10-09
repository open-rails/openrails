package standalonehandlers

// Federated grants (#1140): merchant roles granted by email to users of the
// merchant's trusted issuers, behind /v1/merchant/federated-grants; the
// invitee lists and accepts its pending grants at /v1/merchants/invites.

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// FederatedGrantManager is the control-plane surface behind the federated
// grant routes.
type FederatedGrantManager interface {
	RoleCoveredBy(role iam.Role, grants []string) (bool, error)
	ListFederatedGrants(ctx context.Context, mid billing.MerchantID) ([]billing.FederatedGrant, error)
	CreateFederatedGrant(ctx context.Context, mid billing.MerchantID, email, role string) (billing.FederatedGrant, error)
	FederatedGrant(ctx context.Context, mid billing.MerchantID, id billing.FederatedGrantID) (billing.FederatedGrant, error)
	RevokeFederatedGrant(ctx context.Context, mid billing.MerchantID, id billing.FederatedGrantID) error
	PendingFederatedGrants(ctx context.Context, user *credential.ResourceUser) ([]billing.FederatedInvite, error)
	AcceptFederatedGrant(ctx context.Context, user *credential.ResourceUser, id billing.FederatedGrantID) (billing.UserMerchant, error)
}

// MerchantListFederatedGrants handles GET /v1/merchant/federated-grants.
func MerchantListFederatedGrants(svc FederatedGrantManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		grants, err := svc.ListFederatedGrants(r.Request.Context(), mid)
		if err != nil {
			r.InternalError("list federated grants", err)
			return
		}
		r.SuccessJSON(billing.ListPage[billing.FederatedGrant]{Items: grants})
	}
}

// MerchantCreateFederatedGrant handles POST /v1/merchant/federated-grants
// {email, role}: a pending grant the invitee accepts after signing in at one
// of the merchant's trusted issuers.
func MerchantCreateFederatedGrant(svc FederatedGrantManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		var req billing.CreateFederatedGrantParams
		if !r.BindJSON(&req) {
			return
		}
		role, ok := merchantRole(r, req.Role, controlplane.MerchantRoles())
		if !ok || !grantWithin(r, svc, role) {
			return
		}
		grant, err := svc.CreateFederatedGrant(r.Request.Context(), mid, req.Email, role.Name())
		switch {
		case errors.Is(err, controlplane.ErrFederatedGrantInvalidEmail):
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_email", "email must be a bare address"))
		case errors.Is(err, controlplane.ErrFederatedGrantExists):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, api.CodeResourceConflict, "that email already has a grant on this merchant"))
		case err != nil:
			r.InternalError("create federated grant", err)
		default:
			r.JSON(http.StatusCreated, grant)
		}
	}
}

// MerchantRevokeFederatedGrant handles DELETE /v1/merchant/federated-grants/{id}.
func MerchantRevokeFederatedGrant(svc FederatedGrantManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		id, err := billing.ParseFederatedGrantID(r.Param("id"))
		if err != nil || id.IsZero() {
			grantNotFound(r)
			return
		}
		ctx := r.Request.Context()
		grant, err := svc.FederatedGrant(ctx, mid, id)
		if errors.Is(err, controlplane.ErrFederatedGrantNotFound) {
			grantNotFound(r)
			return
		}
		if err != nil {
			r.InternalError("read federated grant", err)
			return
		}
		role, _ := controlplane.MerchantRole(grant.Role)
		if !grantWithin(r, svc, role) {
			return
		}
		if err := svc.RevokeFederatedGrant(ctx, mid, id); errors.Is(err, controlplane.ErrFederatedGrantNotFound) {
			grantNotFound(r)
			return
		} else if err != nil {
			r.InternalError("revoke federated grant", err)
			return
		}
		r.NoContent()
	}
}

// ListMyFederatedGrants handles GET /v1/merchants/invites: the pending grants
// the signed-in issuer user's verified email may accept.
func ListMyFederatedGrants(svc FederatedGrantManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		user, ok := middleware.ResourceUserFromRequest(r)
		if !ok {
			r.SuccessJSON(billing.ListPage[billing.FederatedInvite]{Items: []billing.FederatedInvite{}})
			return
		}
		invites, err := svc.PendingFederatedGrants(r.Request.Context(), user)
		if err != nil {
			r.InternalError("list pending federated grants", err)
			return
		}
		r.SuccessJSON(billing.ListPage[billing.FederatedInvite]{Items: invites})
	}
}

// AcceptFederatedGrant handles POST /v1/merchants/invites/{id}/accept.
func AcceptFederatedGrant(svc FederatedGrantManager) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		user, ok := middleware.ResourceUserFromRequest(r)
		id, err := billing.ParseFederatedGrantID(r.Param("id"))
		if !ok || err != nil || id.IsZero() {
			grantNotFound(r)
			return
		}
		merchant, err := svc.AcceptFederatedGrant(r.Request.Context(), user, id)
		switch {
		case errors.Is(err, controlplane.ErrFederatedGrantUnverified):
			r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "email_unverified", "the access token carries no verified email"))
		case errors.Is(err, controlplane.ErrFederatedGrantNotFound):
			grantNotFound(r)
		case errors.Is(err, controlplane.ErrFederatedGrantExists):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, api.CodeResourceConflict, "you already hold a grant on this merchant"))
		case err != nil:
			r.InternalError("accept federated grant", err)
		default:
			r.SuccessJSON(merchant)
		}
	}
}

// grantWithin refuses a grant beyond the caller's authority: a credential
// carrying permissions must cover the role's. A member holding
// members:manage is an owner.
func grantWithin(r *httprequest.Request, svc FederatedGrantManager, role iam.Role) bool {
	principal, ok := merchantRoutePrincipal(r)
	if !ok || len(principal.Permissions) == 0 {
		return true
	}
	covered, err := svc.RoleCoveredBy(role, principal.Permissions)
	if err != nil {
		r.InternalError("resolve role permissions", err)
		return false
	}
	if !covered {
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "role_escalation", "cannot grant authority beyond your own"))
		return false
	}
	return true
}

func grantNotFound(r *httprequest.Request) {
	r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound, "no such federated grant"))
}
