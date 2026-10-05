package standalonehandlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
)

// MerchantRenamer is the control-plane surface behind PUT /v1/merchant/name.
type MerchantRenamer interface {
	RenameMerchant(ctx context.Context, mid billing.MerchantID, name, actorUserID string, operator bool) (*merchants.Merchant, error)
}

// MerchantRename handles PUT /v1/merchant/name {"name": …}: renames the
// caller's merchant (#1106). The former name keeps forwarding to it under the
// deployment's naming policy.
func MerchantRename(svc MerchantRenamer) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		mid, ok := teamMerchantScope(r)
		if !ok {
			return
		}
		var req billing.RenameMerchantParams
		if !r.BindJSON(&req) {
			return
		}
		actor := ""
		if uc, ok := r.UserContext(); ok {
			actor = uc.UserID
		}
		m, err := svc.RenameMerchant(r.Request.Context(), mid, req.Name, actor, false)
		var tooSoon *merchants.RenameTooSoonError
		switch {
		case err == nil:
			r.SuccessJSON(billing.MerchantName{ID: m.ID, Name: m.Slug})
		case errors.As(err, &tooSoon):
			r.SetHeader("Retry-After", strconv.Itoa(int(time.Until(tooSoon.NextRenameAt).Seconds())+1))
			r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeInvalidRequest, "rename_too_soon",
				"the next rename is allowed at "+tooSoon.NextRenameAt.UTC().Format(time.RFC3339)))
		case errors.Is(err, billing.ErrMerchantNameTaken):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "name_taken", "that merchant name is taken"))
		case errors.Is(err, billing.ErrMerchantSlugReserved):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "name_reserved", "that merchant name is reserved"))
		case errors.Is(err, merchants.ErrRenamesDisabled):
			r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "renames_disabled", "merchant renames are disabled"))
		case errors.Is(err, merchants.ErrInvalidName):
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_name", err.Error()))
		case errors.Is(err, merchants.ErrMerchantNotFound):
			r.ErrorCode(billing.CodeResourceNotFound, "merchant not found")
		default:
			teamServiceError(r, err, "merchant rename failed")
		}
	}
}
