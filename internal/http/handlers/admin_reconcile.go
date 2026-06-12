package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Admin reconcile surface (#107 phase 2). Manual-only by design (decision 3:
// no scheduled runs): an admin POSTs a run, which executes SYNCHRONOUSLY
// (fetch + diff [+ apply in enforce mode]) and returns the run summary +
// findings. Enforce performs LOCAL writes only — no processor is ever
// mutated, so this works under mode=readonly.

type adminReconcileRunRequest struct {
	Mode      string   `json:"mode"`      // advisory (default) | enforce
	Providers []string `json:"providers"` // empty = all configured
	Since     string   `json:"since"`     // RFC3339 or YYYY-MM-DD
	Until     string   `json:"until"`
}

type adminReconcileFindingPath struct {
	ID string `uri:"id" binding:"required"`
}

type adminReconcileNotesRequest struct {
	Notes string `json:"notes"`
}

func reconcileEngineFromRuntime(r *httprequest.Request) (*reconcile.Engine, *reconcile.PGStore, bool) {
	rt := r.State
	if rt == nil || rt.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "runtime unavailable")
		return nil, nil, false
	}
	fetchers := reconcile.BuildFetchers(rt.Config, reconcile.FetcherClients{
		NMIClients:     rt.NMIClients,
		CCBillDataLink: rt.CCBillDataLink,
		SolanaRPC:      rt.SolanaRPC,
	}, rt.DB)
	if len(fetchers) == 0 {
		r.ErrorJSON(http.StatusServiceUnavailable, "no payment providers configured for reconciliation")
		return nil, nil, false
	}
	return reconcile.NewEngine(rt.DB, rt.Config, fetchers), &reconcile.PGStore{DB: rt.DB}, true
}

func parseReconcileTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// AdminReconcileRun runs a reconciliation synchronously and returns the
// run + summary + findings.
func AdminReconcileRun(r *httprequest.Request) {
	var req adminReconcileRunRequest
	if !r.BindJSON(&req) {
		return
	}
	mode := reconcile.Mode(req.Mode)
	if mode == "" {
		mode = reconcile.ModeAdvisory
	}
	if mode != reconcile.ModeAdvisory && mode != reconcile.ModeEnforce {
		r.ErrorJSON(http.StatusBadRequest, "mode must be advisory or enforce")
		return
	}
	since, err := parseReconcileTime(req.Since)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	until, err := parseReconcileTime(req.Until)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid until: "+err.Error())
		return
	}
	var providers []reconcile.Provider
	for _, p := range req.Providers {
		providers = append(providers, reconcile.Provider(p))
	}

	eng, _, ok := reconcileEngineFromRuntime(r)
	if !ok {
		return
	}
	res, err := eng.Run(r.Request.Context(), reconcile.RunParams{
		Mode:      mode,
		Providers: providers,
		Since:     since,
		Until:     until,
	})
	if err != nil {
		if res != nil {
			// Partial result (e.g. circuit-breaker abort on one provider):
			// surface both the result and the error.
			r.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": res})
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(res)
}

// AdminReconcileGetRun returns one run (header + summary jsonb).
func AdminReconcileGetRun(r *httprequest.Request) {
	var path adminReconcileFindingPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	id, err := uuid.Parse(path.ID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid run id")
		return
	}
	_, store, ok := reconcileEngineFromRuntimeStoreOnly(r)
	if !ok {
		return
	}
	run, err := store.GetRun(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "run not found")
		return
	}
	r.SuccessJSON(run)
}

// AdminReconcileListRuns lists recent runs.
func AdminReconcileListRuns(r *httprequest.Request) {
	_, store, ok := reconcileEngineFromRuntimeStoreOnly(r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.Query("limit"))
	offset, _ := strconv.Atoi(r.Query("offset"))
	runs, err := store.ListRuns(r.Request.Context(), limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSON(runs)
}

// AdminReconcileListFindings lists findings filtered by
// status/provider/type/admin queue.
func AdminReconcileListFindings(r *httprequest.Request) {
	_, store, ok := reconcileEngineFromRuntimeStoreOnly(r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.Query("limit"))
	offset, _ := strconv.Atoi(r.Query("offset"))
	filter := reconcile.FindingFilter{
		Status:         r.Query("status"),
		Provider:       r.Query("provider"),
		Type:           r.Query("type"),
		OnlyAdminQueue: r.Query("admin_queue") == "true",
		Limit:          limit,
		Offset:         offset,
	}
	findings, err := store.ListFindings(r.Request.Context(), filter)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSON(findings)
}

// AdminReconcileAckFinding resolves a finding as admin-acknowledged.
func AdminReconcileAckFinding(r *httprequest.Request) {
	adminReconcileFindingLifecycle(r, func(store *reconcile.PGStore, id uuid.UUID, notes string) (bool, error) {
		return store.AckFinding(r.Request.Context(), id, notes)
	})
}

// AdminReconcileDismissFinding dismisses a finding; dismissed identities stay
// dismissed across re-runs.
func AdminReconcileDismissFinding(r *httprequest.Request) {
	adminReconcileFindingLifecycle(r, func(store *reconcile.PGStore, id uuid.UUID, notes string) (bool, error) {
		return store.DismissFinding(r.Request.Context(), id, notes)
	})
}

func adminReconcileFindingLifecycle(r *httprequest.Request, op func(*reconcile.PGStore, uuid.UUID, string) (bool, error)) {
	var path adminReconcileFindingPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	id, err := uuid.Parse(path.ID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid finding id")
		return
	}
	var body adminReconcileNotesRequest
	if !r.BindJSON(&body) {
		return
	}
	_, store, ok := reconcileEngineFromRuntimeStoreOnly(r)
	if !ok {
		return
	}
	changed, err := op(store, id, body.Notes)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if !changed {
		r.ErrorJSON(http.StatusConflict, "finding not found or not in an actionable state")
		return
	}
	finding, err := store.GetFinding(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSON(finding)
}

// reconcileEngineFromRuntimeStoreOnly builds just the store (reads + finding
// lifecycle need no fetchers, so missing provider credentials must not block
// browsing findings).
func reconcileEngineFromRuntimeStoreOnly(r *httprequest.Request) (*reconcile.Engine, *reconcile.PGStore, bool) {
	rt := r.State
	if rt == nil || rt.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "runtime unavailable")
		return nil, nil, false
	}
	return nil, &reconcile.PGStore{DB: rt.DB}, true
}
