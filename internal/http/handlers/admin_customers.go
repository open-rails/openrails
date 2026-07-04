package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// adminCustomerSummary is one row of the merchant customer list/search (#740).
type adminCustomerSummary struct {
	ID         string    `json:"id"`
	Subject    *string   `json:"subject,omitempty"`
	Email      *string   `json:"email,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

const (
	adminCustomersDefaultLimit = 50
	adminCustomersMaxLimit     = 200
)

// ListAdminCustomers lists/searches the merchant's customers (#740).
// q matches subject (external ref) prefix, customer id prefix, or a
// subscription email substring. Orchestration-free read: handler -> gen on
// the merchant-pinned conn (RLS scopes rows).
func ListAdminCustomers(r *httprequest.Request) {
	ctx := r.Request.Context()
	q := strings.TrimSpace(r.Query("q"))

	limit := adminCustomersDefaultLimit
	if raw := r.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			r.ErrorJSON(http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, adminCustomersMaxLimit)
	}
	offset := 0
	if raw := r.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			r.ErrorJSON(http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}

	queries := r.State.DB.Gen(ctx)
	rows, err := queries.SearchCustomers(ctx, gen.SearchCustomersParams{
		Q:          q,
		PageLimit:  int64(limit),
		PageOffset: int64(offset),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to search customers")
		return
	}
	total, err := queries.CountSearchCustomers(ctx, q)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count customers")
		return
	}
	items := make([]adminCustomerSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminCustomerSummary{
			ID:         row.ID.String(),
			Subject:    row.Subject,
			Email:      row.Email,
			CreatedAt:  row.CreatedAt,
			LastSeenAt: row.LastSeenAt,
		})
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}
