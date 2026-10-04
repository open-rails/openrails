package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// worker_state is global, with no merchant column: last_error is another
// merchant's verbatim job error (slugs, ids), so its text is platform-only
// (#SEC-22). The merchant tier keeps the signal without it.

// GetAdminWorkerHealth lists every registered worker kind with its last
// success/error/streak (#689) — the "expected N runs, got 0" dashboard.
// MERCHANT tier: error text withheld (#SEC-22). Orchestration-free read:
// handler -> gen directly (worker_state is an operator-global control-plane
// table, no merchant scope).
func GetAdminWorkerHealth(r *httprequest.Request) { listWorkerHealth(r, false) }

// GetPlatformWorkerHealth is the same list for a platform operator, with the
// verbatim last_error text (root:worker-health:read, #SEC-22).
func GetPlatformWorkerHealth(r *httprequest.Request) { listWorkerHealth(r, true) }

func listWorkerHealth(r *httprequest.Request, withErrorText bool) {
	ctx := r.Request.Context()
	rows, err := r.State.DB.Gen(ctx).ListWorkerHealth(ctx)
	if err != nil {
		r.InternalError("list worker health failed", err)
		return
	}
	items := make([]billing.WorkerHealth, 0, len(rows))
	for _, row := range rows {
		items = append(items, workerHealthItemFromGen(row, withErrorText))
	}
	r.SuccessJSON(billing.ListPage[billing.WorkerHealth]{Items: items})
}

func workerHealthItemFromGen(row gen.BillingWorkerState, withErrorText bool) billing.WorkerHealth {
	item := billing.WorkerHealth{
		WorkerKind:            row.WorkerKind,
		RegisteredAt:          row.RegisteredAt,
		ExpectedPeriodSeconds: row.ExpectedPeriodSeconds,
		LastSuccessAt:         row.LastSuccessAt,
		LastErrorAt:           row.LastErrorAt,
		ConsecutiveFailures:   row.ConsecutiveFailures,
		LastAlertedAt:         row.LastAlertedAt,
		UpdatedAt:             row.UpdatedAt,
	}
	if withErrorText {
		item.LastError = row.LastError
	}
	return item
}
