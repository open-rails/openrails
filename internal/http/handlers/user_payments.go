package handlers

import (
	"net/http"
	"strconv"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/query"
)

func GetUserPayments(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	paymentType := r.Request.URL.Query().Get("type")

	queryOpts := &query.QueryOptions[payments.GetPaymentsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: payments.GetPaymentsFilters{},
	}

	if paymentType != "" {
		queryOpts.Filters.Processor = paymentType
	}

	payments, _, err := r.State.UserSubscriptionService.GetUserPayments(
		r.Request.Context(),
		user.ID,
		queryOpts,
	)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSONPaginated(payments, queryOpts.TotalItems, limit, offset)
}
