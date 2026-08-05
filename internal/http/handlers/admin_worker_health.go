package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// workerHealthItem is the view of one registered River worker kind (#689).
//
// #SEC-22: worker_health is deliberately RLS-exempt and has NO merchant column
// — every merchant's rows sit in it. last_error is the verbatim Go error string
// of some merchant's job and routinely embeds slugs, subscription/customer
// UUIDs and PSP account ids, so the TEXT is platform-only. The merchant tier
// still gets the signal it needs (whether a kind is erroring, when, and the
// streak) without another merchant's error text.
type workerHealthItem struct {
	WorkerKind            string     `json:"worker_kind"`
	RegisteredAt          time.Time  `json:"registered_at"`
	ExpectedPeriodSeconds *int64     `json:"expected_period_seconds,omitempty"`
	LastSuccessAt         *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt           *time.Time `json:"last_error_at,omitempty"`
	LastError             *string    `json:"last_error,omitempty"`
	ConsecutiveFailures   int32      `json:"consecutive_failures"`
	LastAlertedAt         *time.Time `json:"last_alerted_at,omitempty"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// GetAdminWorkerHealth lists every registered worker kind with its last
// success/error/streak (#689) — the "expected N runs, got 0" dashboard.
// MERCHANT tier: error text withheld (#SEC-22). Orchestration-free read:
// handler -> gen directly (worker_health is an operator-global control-plane
// table, no merchant scope).
func GetAdminWorkerHealth(r *httprequest.Request) { listWorkerHealth(r, false) }

// GetPlatformWorkerHealth is the same list for a platform operator, with the
// verbatim last_error text (root:worker-health:read, #SEC-22).
func GetPlatformWorkerHealth(r *httprequest.Request) { listWorkerHealth(r, true) }

func listWorkerHealth(r *httprequest.Request, withErrorText bool) {
	ctx := r.Request.Context()
	rows, err := r.State.DB.Gen(ctx).ListWorkerHealth(ctx)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve worker health")
		return
	}
	items := make([]workerHealthItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, workerHealthItemFromGen(row, withErrorText))
	}
	r.SuccessJSON(items)
}

func workerHealthItemFromGen(row gen.OpenrailsWorkerHealth, withErrorText bool) workerHealthItem {
	item := workerHealthItem{
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
