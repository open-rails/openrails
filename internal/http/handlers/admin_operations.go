package handlers

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
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
	} else if offset > math.MaxInt32 {
		offset = math.MaxInt32
	}
	return limit, offset
}

func GetAdminRepairAlerts(r *httprequest.Request) {
	ctx := r.Request.Context()
	limit, offset := adminOperationsPagination(r)
	tsid := db.SystemCustomerID

	var seen *bool
	seenParam := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("seen")))
	if seenParam == "true" || seenParam == "false" {
		v := seenParam == "true"
		seen = &v
	}

	q := r.State.DB.Gen(ctx)
	total, err := q.CountRepairAlerts(ctx, gen.CountRepairAlertsParams{
		CustomerID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count repair alerts")
		return
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListRepairAlerts(ctx, gen.ListRepairAlertsParams{
		CustomerID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
		Column3: limit32, Column4: offset32,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve repair alerts")
		return
	}
	items := make([]*models.NotificationQueue, 0, len(rows))
	for _, row := range rows {
		m, merr := models.NotificationFromGen(row)
		if merr != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to decode repair alerts")
			return
		}
		items = append(items, m)
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}

// #528: GetAdminProviderIntents (the #358 provider-intent ledger debug view) was
// dropped — it lived only on the retired per-user admin surface.
// #666: GetAdminManualRebillAttempts (never routed) was dropped with it.
