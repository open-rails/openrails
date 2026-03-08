package handlers

import (
	"net/http"
	"strconv"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
)

// GetAdminUserPayments returns paginated payments for a user
// GET /v1/admin/users/:user_id/payments?page=1&page_size=50
func GetAdminUserPayments(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	// Pagination params (page-based)
	page := 1
	pageSize := 50

	if p := r.GinCtx.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := r.GinCtx.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 200 {
			pageSize = v
		}
	}

	payments, total, err := r.State.PaymentService.GetPaginatedByUserID(r.Request.Context(), path.UserID, page, pageSize)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	data := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		data[i] = PaymentToAPI(p, nil)
	}

	offset := (page - 1) * pageSize
	hasMore := offset+len(data) < total

	r.GinCtx.JSON(http.StatusOK, map[string]interface{}{
		"object":   "list",
		"data":     data,
		"total":    total,
		"limit":    pageSize,
		"offset":   offset,
		"has_more": hasMore,
	})
}
