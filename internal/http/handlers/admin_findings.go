package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// #692 operator findings queue. The reconciliation_findings ledger IS the
// queue: these endpoints list it, show one item, and resolve one item at a
// time (approve executes the structured recommendation; ignore silences the
// subject permanently). Deliberately NO bulk endpoint — bulk destructive ops
// are what the #679 breaker guards against.

func findingsStore(r *httprequest.Request) (*reconcile.PGStore, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "findings ledger unavailable")
		return nil, false
	}
	return &reconcile.PGStore{DB: r.State.DB}, true
}

// findingView is one queue item: the persisted record plus the parsed
// structured recommendation (nil when the finding has no mechanical fix —
// approve is unavailable for those).
type findingView struct {
	reconcile.FindingRecord
	Recommendation *recommend.Recommendation `json:"recommendation,omitempty"`
}

func newFindingView(rec reconcile.FindingRecord) findingView {
	v := findingView{FindingRecord: rec}
	if parsed, ok := recommend.FromEvidence(rec.Evidence); ok {
		v.Recommendation = &parsed
	}
	return v
}

type findingsListResponse struct {
	Items  []findingView         `json:"items"`
	Total  int64                 `json:"total"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
	Gauges reconcile.QueueGauges `json:"gauges"`
}

// AdminListFindings lists the operator work list (open findings by default),
// sorted severity desc then age desc, with the #690 gauge summary.
//
// Gauge alert semantics (#690, three error categories): `orphaned_members`
// (paying without access — MISSING side), `freeloaders` (access without
// paying — EXCESS side) and `duplicate_coverage` (double-billed) are
// always-zero error metrics — nonzero for a full sweep cycle (15 min) means
// the billing state machine is failing (an undelivered paid grant, a broken
// justification chain, or a double charge that detection re-confirmed).
// Severity ordering (Paul): taking money wrongly (orphaned, double-billed =
// critical) outranks giving content away (freeloader = high).
// `verification_pressure` is a live pressure reading over `unknown` subs past
// paid-through — nonzero is allowed (fail-open standing access awaiting
// provider verification), but its max_age_seconds trending UP means the
// verification machinery (pull/probe/converge) is down. `episodes` is the
// historical/interval companion: freeloader/orphaned SPANS with total
// error-days from the migration-067 views (open episodes still accrue;
// sanctioned freeloader spans — dunning, awaiting verification — are policy
// and only the `unsanctioned` split indicates failure).
//
//	GET /merchant/findings?severity=&finding_type=&status=&limit=&offset=
func AdminListFindings(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	filter := reconcile.QueueFilter{
		Severity: strings.TrimSpace(r.Query("severity")),
		Type:     strings.TrimSpace(r.Query("finding_type")),
		Status:   strings.TrimSpace(r.Query("status")),
		Limit:    parseIntDefault(r.Query("limit"), 100),
		Offset:   parseIntDefault(r.Query("offset"), 0),
	}
	items, total, err := store.ListQueueFindings(ctx, filter)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list findings")
		return
	}
	gauges, err := store.Gauges(ctx)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to compute finding gauges")
		return
	}
	views := make([]findingView, 0, len(items))
	for _, item := range items {
		views = append(views, newFindingView(item))
	}
	r.JSON(http.StatusOK, findingsListResponse{
		Items:  views,
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
		Gauges: gauges,
	})
}

// AdminGetFinding returns one finding with full evidence + recommendation.
//
//	GET /merchant/findings/{id}
func AdminGetFinding(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid finding id")
		return
	}
	finding, err := store.GetFinding(r.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "finding not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to load finding")
		return
	}
	r.JSON(http.StatusOK, newFindingView(finding))
}

type resolveFindingRequest struct {
	Outcome string `json:"outcome"`
	Notes   string `json:"notes"`
	// RAW on purpose (or#863): plain binding decodes JSON numbers as float64,
	// and `amount` here is a refund in MICROS heading for a real provider
	// refund. Decoded through recommend.DecodeParams (UseNumber) instead.
	OverrideParams json.RawMessage `json:"override_params,omitempty"`
}

type resolveFindingResponse struct {
	Finding   findingView    `json:"finding"`
	Execution map[string]any `json:"execution,omitempty"`
}

func findingIsOpen(f reconcile.FindingRecord) bool {
	return f.Status == reconcile.FindingStatusRequiresReview ||
		f.Status == reconcile.FindingStatusReconcileRequired
}

// resolveActorIdentity is the stamped resolved_by: the authenticated admin's
// user id (or email). Service credentials carry no user identity — recorded
// as the generic label.
func resolveActorIdentity(r *httprequest.Request) string {
	if uc, ok := r.UserContext(); ok {
		if strings.TrimSpace(uc.UserID) != "" {
			return uc.UserID
		}
		if strings.TrimSpace(uc.Email) != "" {
			return uc.Email
		}
	}
	return "service-credential"
}

// AdminResolveFinding resolves ONE finding: approve executes the structured
// recommendation (params merged with override_params) then marks it
// fixed/admin_fixed; ignore silences the subject permanently (notes required).
// Partial execution failure leaves the finding OPEN with the error appended
// to notes. The next converge/pull sweep re-measures — a fix that didn't take
// reopens by re-detection, never by trust.
//
//	POST /merchant/findings/{id}/resolve  {outcome, notes, override_params?}
func AdminResolveFinding(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid finding id")
		return
	}
	var req resolveFindingRequest
	if !r.BindJSON(&req) {
		return
	}
	outcome := strings.ToLower(strings.TrimSpace(req.Outcome))
	if outcome != "approve" && outcome != "ignore" {
		r.ErrorJSON(http.StatusBadRequest, "outcome must be \"approve\" or \"ignore\"")
		return
	}
	notes := strings.TrimSpace(req.Notes)

	finding, err := store.GetFinding(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "finding not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to load finding")
		return
	}
	if !findingIsOpen(finding) {
		// Idempotent no-op: already resolved; report the current state.
		r.ErrorJSON(http.StatusConflict, "finding already resolved (status="+string(finding.Status)+", resolution="+finding.Resolution+")")
		return
	}
	actor := resolveActorIdentity(r)

	if outcome == "ignore" {
		if notes == "" {
			r.ErrorJSON(http.StatusBadRequest, "notes are required to ignore a finding (permanent silence for this subject)")
			return
		}
		okRow, err := store.IgnoreFindingWithActor(ctx, id, notes, actor)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to ignore finding")
			return
		}
		if !okRow {
			r.ErrorJSON(http.StatusConflict, "finding is no longer open")
			return
		}
		updated, err := store.GetFinding(ctx, id)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "finding ignored but failed to reload")
			return
		}
		r.JSON(http.StatusOK, resolveFindingResponse{Finding: newFindingView(updated)})
		return
	}

	// approve: mechanical execution requires a structured recommendation.
	rec, hasRec := recommend.FromEvidence(finding.Evidence)
	if !hasRec {
		r.ErrorJSON(http.StatusUnprocessableEntity,
			"finding carries no structured recommendation; approve is unavailable — resolve with outcome=ignore or fix out-of-band")
		return
	}
	overrides, err := recommend.DecodeParams(req.OverrideParams)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid override_params: "+err.Error())
		return
	}
	params := recommend.MergeParams(rec.Params, overrides)
	execution, execErr := executeFindingAction(r, finding, rec.Action, params, notes)
	if execErr != nil {
		// PARTIAL FAILURE: the finding stays OPEN; the error (plus whatever
		// completed — the compensation state) is appended to operator notes.
		note := "approve by " + actor + " failed: " + execErr.Error()
		if len(execution) > 0 {
			note += " (completed: " + compactJSON(execution) + ")"
		}
		if nerr := store.AppendFindingNotes(ctx, id, note); nerr != nil {
			r.ErrorJSON(http.StatusInternalServerError, "execution failed and the failure note could not be recorded: "+execErr.Error())
			return
		}
		r.ErrorJSON(findingActionErrorStatus(execErr), "recommendation execution failed; finding remains open: "+execErr.Error())
		return
	}
	okRow, err := store.ResolveFindingFixed(ctx, id, notes, actor, execution)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "recommendation executed but finding could not be marked fixed")
		return
	}
	if !okRow {
		r.ErrorJSON(http.StatusConflict, "recommendation executed but the finding was no longer open")
		return
	}
	updated, err := store.GetFinding(ctx, id)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "finding resolved but failed to reload")
		return
	}
	r.JSON(http.StatusOK, resolveFindingResponse{Finding: newFindingView(updated), Execution: execution})
}
