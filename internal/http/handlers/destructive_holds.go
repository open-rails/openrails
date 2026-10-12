package handlers

import (
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"strings"
)

func ListDestructiveHolds(r *httprequest.Request) {
	if r.State == nil || r.State.DestructiveAdmissions == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "")
		return
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorCode(billing.CodePermissionRequired, "")
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return
	}
	var cursor struct {
		ID uuid.UUID `json:"i"`
	}
	present, err := pagination.Decode(page.Cursor, &cursor)
	if err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	var after *uuid.UUID
	if present {
		after = &cursor.ID
	}
	holds, err := r.State.DestructiveAdmissions.ListHolds(r.Request.Context(), mid.UUID(), limit+1, after)
	if err != nil {
		writeRefusal(r, err, "list destructive holds failed")
		return
	}
	items := make([]billing.DestructiveHold, 0, len(holds))
	for _, hold := range holds {
		items = append(items, hold.View())
	}
	r.SuccessJSON(pagination.Cut(items, limit, func(h billing.DestructiveHold) any {
		return struct {
			ID uuid.UUID `json:"i"`
		}{h.ID.UUID()}
	}))
}

// ClearDestructiveHold is invoked only after the explicit operator/human gate.
func ClearDestructiveHold(r *httprequest.Request) {
	if r.State == nil || r.State.DestructiveAdmissions == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "")
		return
	}
	id, ok := pathID(r, billing.ParseDestructiveHoldID)
	if !ok {
		return
	}
	var params billing.ClearDestructiveHoldParams
	if !r.BindJSON(&params) {
		return
	}
	if strings.TrimSpace(params.Reason) == "" || len(params.Reason) > 2000 {
		r.ErrorCode(billing.CodeInvalidParam, "clear reason is required (at most 2000 bytes)")
		return
	}
	actor, err := destructive.ActorFromContext(r.Request.Context())
	if err != nil {
		r.ErrorCode(billing.CodePermissionRequired, "")
		return
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorCode(billing.CodePermissionRequired, "")
		return
	}
	hold, err := r.State.DestructiveAdmissions.Clear(r.Request.Context(), mid.UUID(), id.UUID(), actor, params.Reason)
	switch {
	case errors.Is(err, destructive.ErrSelfClear):
		r.ErrorCode(billing.CodePermissionRequired, "an incident cannot be cleared by its triggering actor")
	case errors.Is(err, pgx.ErrNoRows):
		r.ErrorCode(billing.CodeResourceNotFound, "")
	case errors.Is(err, destructive.ErrConflict):
		r.ErrorCode(billing.CodeResourceConflict, "")
	case err != nil:
		writeRefusal(r, err, "clear destructive hold failed")
	default:
		r.SuccessJSON(hold.View())
	}
}
