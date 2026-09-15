package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/billingimport"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ImportDeclaredBilling handles POST /v1/import/billing (#737): the HTTP door
// into the DeclaredBilling import seam (billingimport.Import — the same body/
// result vocabulary as pkg/embedded.ImportBilling). The merchant comes from the
// authenticated credential; times are RFC3339, amounts provider-wire CENTS
// (amount_cents). Idempotency and batching semantics are the seam's: re-posting
// the same book at the same as_of is a no-op, and the global body cap forces
// large books to batch — batched calls MUST NOT set subscriptions_exhaustive
// (absence reasoning is only valid over a whole book in one call).
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
		r.ErrorJSON(http.StatusInternalServerError, "merchant unresolved")
		return
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing import unavailable")
		return
	}
	res, err := billingimport.Import(r.Request.Context(), billingimport.Options{
		Config:     r.State.Config,
		PGXPool:    r.State.DB.Pool(),
		MerchantID: mid,
		Book:       book,
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "required") || strings.Contains(msg, "invalid") {
			r.ErrorJSON(http.StatusBadRequest, msg)
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, msg)
		return
	}
	r.JSON(http.StatusOK, res)
}
