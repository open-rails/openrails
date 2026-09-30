package standalonehandlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantLister is the control-plane surface behind GET /v1/merchants.
type MerchantLister interface {
	ListUserMerchants(ctx context.Context, userID string) ([]controlplane.UserMerchant, error)
}

// MerchantListMine handles GET /v1/merchants: the live merchants the signed-in
// user holds a role in, with its highest role in each (#1106).
func MerchantListMine(svc MerchantLister) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		uc, ok := r.UserContext()
		if !ok || strings.TrimSpace(uc.UserID) == "" {
			r.ErrorJSON(http.StatusUnauthorized, "authentication required")
			return
		}
		list, err := svc.ListUserMerchants(r.Request.Context(), uc.UserID)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to list merchants")
			return
		}
		r.JSON(http.StatusOK, map[string]any{"object": "list", "data": list})
	}
}

// MerchantCreator is the control-plane surface behind POST /v1/merchants.
type MerchantCreator interface {
	CreateOwnedMerchant(ctx context.Context, name, userID string) (*merchants.Merchant, bool, error)
	SetMerchantDisplayName(ctx context.Context, id merchant.ID, displayName string) error
}

// MerchantCreate handles POST /v1/merchants {"name", "display_name"?}: the
// signed-in user creates a merchant they own (hosted "registration is
// provisioning", #1106). 201 on creation; 200 when the user already owns the
// merchant the name resolves to, the idempotent repair.
func MerchantCreate(svc MerchantCreator) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		uc, ok := r.UserContext()
		if !ok || strings.TrimSpace(uc.UserID) == "" {
			r.ErrorJSON(http.StatusUnauthorized, "authentication required")
			return
		}
		var req struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		}
		if !r.BindJSON(&req) {
			return
		}
		ctx := r.Request.Context()
		m, created, err := svc.CreateOwnedMerchant(ctx, req.Name, uc.UserID)
		if err != nil {
			merchantCreateError(r, err)
			return
		}
		if err := svc.SetMerchantDisplayName(ctx, m.ID, req.DisplayName); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "set merchant display name failed")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		r.JSON(status, map[string]any{"id": m.ID.String(), "slug": m.Slug, "created": created})
	}
}

func merchantCreateError(r *httprequest.Request, err error) {
	refuse := func(status int, code, message string) {
		r.APIError(api.NewAPIError(status, api.ErrorTypeInvalidRequest, code, message))
	}
	switch {
	case errors.Is(err, merchants.ErrInvalidName):
		refuse(http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, merchants.ErrMerchantNameTaken):
		refuse(http.StatusConflict, "name_taken", "that merchant name is taken")
	case errors.Is(err, controlplane.ErrMerchantSlugReserved):
		refuse(http.StatusConflict, "name_reserved", "that merchant name is reserved")
	case errors.Is(err, controlplane.ErrMerchantCreationEmailUnverified):
		refuse(http.StatusForbidden, "email_unverified", controlplane.ErrMerchantCreationEmailUnverified.Error())
	case errors.Is(err, controlplane.ErrMerchantCreationPaymentMethodRequired):
		refuse(http.StatusPaymentRequired, "payment_method_required", controlplane.ErrMerchantCreationPaymentMethodRequired.Error())
	case errors.Is(err, controlplane.ErrMerchantCreationRefused):
		refuse(http.StatusForbidden, "creation_refused", "merchant creation refused")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "merchant creation failed")
	}
}
