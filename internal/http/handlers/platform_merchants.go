package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	log "github.com/sirupsen/logrus"
)

// Platform merchant directory handlers (#721; openrails-saas #16): the
// cross-merchant operator surface. Orchestration-free reads go straight to gen
// (#688); soft-delete/restore are single-row directory tombstone flips —
// DIRECTORY state only (list exclusion + merchant-auth resolution failure via
// the existing deleted_at IS NULL filters), never the #225 gated purge.

// PlatformMerchant is the operator directory view of one merchant.
type PlatformMerchant struct {
	ID          billing.MerchantID `json:"id"`
	Slug        string             `json:"slug"`
	Status      string             `json:"status"`
	DisplayName *string            `json:"display_name"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	DeletedAt   *time.Time         `json:"deleted_at"`
	// RailsArmed is the distinct rails of the merchant's live PSPs: the
	// declared accounts, not a live credential probe.
	RailsArmed []string `json:"rails_armed"`
	// LastPaymentAt is when the merchant's latest payment was recorded.
	LastPaymentAt *time.Time `json:"last_payment_at"`
}

// PlatformMerchantListQuery filters the directory: Status active (default),
// deleted or all; Query matches a substring of the current name.
type PlatformMerchantListQuery struct {
	Status string `form:"status"`
	Query  string `form:"q"`
}

// PlatformListMerchants is GET /v1/platform/merchants (root:merchants:read):
// one page of the directory, newest first.
func PlatformListMerchants(r *httprequest.Request) {
	ctx := r.Request.Context()
	var q PlatformMerchantListQuery
	if !r.BindQuery(&q) {
		return
	}
	var statusFilter *string
	switch q.Status {
	case "", "active":
		active := "active"
		statusFilter = &active
	case "deleted":
		statusFilter = &q.Status
	case "all":
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "status must be active, deleted or all").WithParam("status"))
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	var query *string
	if trimmed := strings.TrimSpace(q.Query); trimmed != "" {
		query = &trimmed
	}
	rows, err := r.State.DB.Gen(ctx).ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{
		Status: statusFilter, Query: query, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(page.Limit),
	})
	if err != nil {
		r.InternalError("failed to list merchants", err)
		return
	}
	cut := pagination.Cut(rows, page.Limit, func(row gen.ListPlatformMerchantsRow) any { return pagination.TimeID{At: row.CreatedAt, ID: row.ID} })
	out := billing.ListPage[PlatformMerchant]{Items: make([]PlatformMerchant, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		item := PlatformMerchant{ID: billing.MerchantID(row.ID), Slug: row.Slug, Status: row.Status, DisplayName: row.DisplayName,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, DeletedAt: row.DeletedAt}
		if err := enrichPlatformMerchant(ctx, r.State.DB, row.ID, &item); err != nil {
			r.InternalError("failed to load merchant activity", err)
			return
		}
		out.Items = append(out.Items, item)
	}
	logPlatformMerchantAccess(r, "list", "")
	r.SuccessJSON(out)
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
		r.ErrorCode(billing.CodeResourceNotFound, "merchant not found")
		return
	}
	if err != nil {
		r.InternalError("failed to load merchant", err)
		return
	}
	item := PlatformMerchant{
		ID:          billing.MerchantID(row.ID),
		Slug:        row.Slug,
		Status:      row.Status,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		DeletedAt:   row.DeletedAt,
	}
	if err := enrichPlatformMerchant(ctx, r.State.DB, row.ID, &item); err != nil {
		r.InternalError("failed to load merchant activity", err)
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
		r.ErrorCode(billing.CodeResourceNotFound, "merchant not found")
		return
	}
	if err != nil {
		r.InternalError("failed to delete merchant", err)
		return
	}
	logPlatformMerchantAccess(r, "soft-delete", row.Slug)
	r.SuccessJSON(PlatformMerchant{
		ID:          billing.MerchantID(row.ID),
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
		r.ErrorCode(billing.CodeResourceNotFound, "merchant not found")
		return
	}
	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23514" {
			r.ErrorCode(billing.CodeResourceConflict, "a retired or purged merchant cannot be restored")
			return
		}
		if errors.As(err, &constraint) && constraint.Code == "23505" {
			r.ErrorCode("name_taken", "that merchant name is taken")
			return
		}
		r.InternalError("failed to restore merchant", err)
		return
	}
	logPlatformMerchantAccess(r, "restore", row.Slug)
	r.SuccessJSON(PlatformMerchant{
		ID:          billing.MerchantID(row.ID),
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
func enrichPlatformMerchant(ctx context.Context, d *db.DB, id uuid.UUID, item *PlatformMerchant) error {
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
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid merchant id").WithParam("id"))
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
