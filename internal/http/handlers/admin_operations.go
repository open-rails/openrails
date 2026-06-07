package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

const defaultAdminOperationsLimit = 50

func adminOperationsPagination(r *httprequest.Request) (int, int) {
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = defaultAdminOperationsLimit
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func GetAdminRepairAlerts(r *httprequest.Request) {
	limit, offset := adminOperationsPagination(r)
	items := []*models.NotificationQueue{}
	tsid, err := repo.ResolveTenantSubjectID(r.Request.Context(), r.State.DB.Q(r.Request.Context()), uuid.Nil, "system")
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve tenant subject")
		return
	}
	q := r.State.DB.Q(r.Request.Context()).NewSelect().Model(&items).
		Where("nq.tenant_subject_id = ?", tsid).
		Where("nq.event_type = ?", models.NotificationSystemAlert).
		Where("nq.data ->> 'kind' = ?", "billing_ledger_repair_required")

	seenParam := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("seen")))
	if seenParam == "true" || seenParam == "false" {
		q = q.Where("nq.seen = ?", seenParam == "true")
	}

	total, err := q.Count(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count repair alerts")
		return
	}
	if err := q.OrderExpr("nq.created_at DESC").Limit(limit).Offset(offset).Scan(r.Request.Context()); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve repair alerts")
		return
	}
	r.SuccessJSONPaginated(items, int64(total), limit, offset)
}

func GetAdminManualRebillAttempts(r *httprequest.Request) {
	limit, offset := adminOperationsPagination(r)
	items := []*models.ManualRebillAttempt{}
	q := r.State.DB.Q(r.Request.Context()).NewSelect().Model(&items)

	status := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("status")))
	if status == "" {
		status = string(models.ManualRebillAttemptUnknown)
	}
	if status != "all" {
		switch models.ManualRebillAttemptStatus(status) {
		case models.ManualRebillAttemptPending, models.ManualRebillAttemptSucceeded, models.ManualRebillAttemptFailed, models.ManualRebillAttemptUnknown:
			q = q.Where("mra.status = ?", status)
		default:
			r.ErrorJSON(http.StatusBadRequest, "invalid status")
			return
		}
	}

	processor := strings.TrimSpace(r.Request.URL.Query().Get("processor"))
	if processor != "" {
		q = q.Where("mra.processor = ?", processor)
	}

	total, err := q.Count(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count manual rebill attempts")
		return
	}
	if err := q.OrderExpr("mra.updated_at DESC").Limit(limit).Offset(offset).Scan(r.Request.Context()); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve manual rebill attempts")
		return
	}
	r.SuccessJSONPaginated(items, int64(total), limit, offset)
}
