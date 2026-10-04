package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	log "github.com/sirupsen/logrus"
)

// Platform merchant directory handlers (#721; openrails-saas #16): the
// cross-merchant operator surface. Orchestration-free reads go straight to gen
// (#688); soft-delete/restore are single-row directory tombstone flips —
// DIRECTORY state only (list exclusion + merchant-auth resolution failure via
// the existing deleted_at IS NULL filters), never the #225 gated purge.

// platformMerchantItem is the operator directory view of one merchant.
type PlatformMerchantItem struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Status      string     `json:"status"`
	DisplayName *string    `json:"display_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	// RailsArmed is the distinct non-archived psps rails —
	// the operator-declared account catalog, not a live credential probe.
	RailsArmed []string `json:"rails_armed"`
	// LastPaymentAt is the cheap last-activity proxy: latest payments.created_at
	// (index probe per row). Money movement is the ops-meaningful signal here.
	LastPaymentAt *time.Time `json:"last_payment_at,omitempty"`
}

type PlatformMerchantListQuery struct {
	// Status filters the directory: active (default — soft-deleted excluded),
	// deleted, or all.
	Status string `form:"status"`
	// Query matches a substring of the current merchant name.
	Query  string `form:"q"`
	Limit  int    `form:"limit"`
	Offset int    `form:"offset"`
}

// PlatformListMerchants is GET /v1/platform/merchants (root:merchants:read);
// ?q= searches current merchant names.
// Stable ordering: created_at DESC, id DESC.
func PlatformListMerchants(r *httprequest.Request) {
	ctx := r.Request.Context()
	q := PlatformMerchantListQuery{Status: "active", Limit: 50}
	if err := r.ShouldBindQuery(&q); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if q.Status == "" {
		q.Status = "active"
	}
	var statusFilter *string
	switch q.Status {
	case "active", "deleted":
		statusFilter = &q.Status
	case "all":
		statusFilter = nil
	default:
		r.ErrorJSON(http.StatusBadRequest, "status must be active, deleted, or all")
		return
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	var query *string
	if trimmed := strings.TrimSpace(q.Query); trimmed != "" {
		query = &trimmed
	}
	queries := r.State.DB.Gen(ctx)
	total, err := queries.CountPlatformMerchants(ctx, gen.CountPlatformMerchantsParams{Status: statusFilter, Query: query})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count merchants")
		return
	}
	rows, err := queries.ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{
		Status:     statusFilter,
		Query:      query,
		PageLimit:  int64(q.Limit),
		PageOffset: int64(q.Offset),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list merchants")
		return
	}

	items := make([]PlatformMerchantItem, 0, len(rows))
	for _, row := range rows {
		item := PlatformMerchantItem{
			ID:          row.ID.String(),
			Slug:        row.Slug,
			Status:      row.Status,
			DisplayName: row.DisplayName,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   row.UpdatedAt,
			DeletedAt:   row.DeletedAt,
		}
		if err := enrichPlatformMerchant(ctx, r.State.DB, row.ID, &item); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to load merchant activity")
			return
		}
		items = append(items, item)
	}
	logPlatformMerchantAccess(r, "list", "")
	r.SuccessJSONPaginated(items, total, q.Limit, q.Offset)
}

// PlatformGetMerchant is GET /v1/platform/merchants/:id (root:merchants:read).
// Returns the row in ANY status: operators inspect soft-deleted merchants.
func PlatformGetMerchant(r *httprequest.Request) {
	ctx := r.Request.Context()
	id, ok := platformMerchantPathID(r)
	if !ok {
		return
	}
	row, err := r.State.DB.Gen(ctx).GetPlatformMerchant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "merchant not found")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load merchant")
		return
	}
	item := PlatformMerchantItem{
		ID:          row.ID.String(),
		Slug:        row.Slug,
		Status:      row.Status,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		DeletedAt:   row.DeletedAt,
	}
	if err := enrichPlatformMerchant(ctx, r.State.DB, row.ID, &item); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load merchant activity")
		return
	}
	logPlatformMerchantAccess(r, "get", row.Slug)
	r.SuccessJSON(item)
}

// PlatformSoftDeleteMerchant is DELETE /v1/platform/merchants/:id
// (root:merchants:delete). SOFT delete only: tombstones the directory row
// (status='deleted', deleted_at kept from a prior delete), which drops the
// merchant from default list views and fails merchant-scoped credential
// resolution (deleted_at IS NULL filters in controlplane). All business rows
// are preserved. Idempotent: re-deleting returns the unchanged tombstone.
func PlatformSoftDeleteMerchant(r *httprequest.Request) {
	ctx := r.Request.Context()
	id, ok := platformMerchantPathID(r)
	if !ok {
		return
	}
	row, err := r.State.DB.Gen(ctx).SoftDeletePlatformMerchant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "merchant not found")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to delete merchant")
		return
	}
	logPlatformMerchantAccess(r, "soft-delete", row.Slug)
	r.SuccessJSON(PlatformMerchantItem{
		ID:          row.ID.String(),
		Slug:        row.Slug,
		Status:      row.Status,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		DeletedAt:   row.DeletedAt,
		RailsArmed:  []string{},
	})
}

// PlatformRestoreMerchant is POST /v1/platform/merchants/:id/restore
// (root:merchants:restore). Clears the tombstone; idempotent on active rows.
func PlatformRestoreMerchant(r *httprequest.Request) {
	ctx := r.Request.Context()
	id, ok := platformMerchantPathID(r)
	if !ok {
		return
	}
	row, err := r.State.DB.Gen(ctx).RestorePlatformMerchant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "merchant not found")
		return
	}
	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23514" {
			r.ErrorJSON(http.StatusConflict, "retired or purged merchant cannot be restored")
			return
		}
		if errors.As(err, &constraint) && constraint.Code == "23505" {
			r.ErrorJSON(http.StatusConflict, "merchant name is taken")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to restore merchant")
		return
	}
	logPlatformMerchantAccess(r, "restore", row.Slug)
	r.SuccessJSON(PlatformMerchantItem{
		ID:          row.ID.String(),
		Slug:        row.Slug,
		Status:      row.Status,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		DeletedAt:   row.DeletedAt,
		RailsArmed:  []string{},
	})
}

// enrichPlatformMerchant fills rails-armed + last-activity under a MerchantTx:
// psps and payments are merchant-owned, so each probe is a per-merchant query
// (one tiny tx per directory row — page-bounded, both queries indexed).
func enrichPlatformMerchant(ctx context.Context, d *db.DB, id uuid.UUID, item *PlatformMerchantItem) error {
	item.RailsArmed = []string{}
	mctx := merchant.WithID(ctx, billing.MerchantID(id))
	return d.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		rails, err := q.ListPlatformMerchantRailsArmed(ctx, id)
		if err != nil {
			return err
		}
		if rails != nil {
			item.RailsArmed = rails
		}
		last, err := q.GetPlatformMerchantLastPayment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no payments yet — last_payment_at stays null
		}
		if err != nil {
			return err
		}
		item.LastPaymentAt = &last
		return nil
	})
}

func platformMerchantPathID(r *httprequest.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid merchant id")
		return uuid.UUID{}, false
	}
	return id, true
}

// logPlatformMerchantAccess audits the sensitive cross-merchant operation with
// the acting operator (SearchMerchants doctrine: the caller audits).
func logPlatformMerchantAccess(r *httprequest.Request, action, slug string) {
	operator := ""
	if uc, ok := r.UserContext(); ok {
		operator = uc.UserID
	}
	log.WithFields(log.Fields{
		"operator": operator,
		"action":   action,
		"merchant": slug,
	}).Info("platform merchant directory access")
}
