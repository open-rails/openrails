package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingimport"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
)

// ImportDeclaredBilling handles POST /v1/admin/billing-import: the HTTP door to
// billingimport.Import (Client.ImportBilling's body and result). The merchant
// is the credential's. Re-posting the same book at the same as_of is a no-op;
// the body cap forces large books into batches, which must not set
// subscriptions_exhaustive (absence is only provable over a whole book).
func ImportDeclaredBilling(r *httprequest.Request) {
	var book billingimport.DeclaredBilling
	if !r.BindJSON(&book) {
		return
	}
	if book.AsOf.IsZero() {
		r.APIError(&api.APIError{
			HTTPStatus: http.StatusBadRequest,
			Type:       api.ErrorTypeInvalidRequest,
			Code:       "as_of_required",
			Message:    "as_of (RFC3339 evidence horizon) is required",
		})
		return
	}
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeInternalError, "merchant unresolved")
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeInternalError, "billing import unavailable")
		return
	}
	res, err := billingimport.Import(r.Request.Context(), billingimport.Options{
		DB:         r.State.DB,
		MerchantID: mid,
		Book:       book,
		Clock:      r.Clock,
	})
	if err != nil {
		writeRefusal(r, err, "billing import failed")
		return
	}
	r.JSON(http.StatusOK, res)
}
