package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
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
		r.ErrorCode(billing.CodeServiceUnavailable, "findings ledger unavailable")
		return nil, false
	}
	store := &reconcile.PGStore{DB: r.State.DB}
	if r.Clock != nil {
		store.Now = r.Clock.Now
	}
	return store, true
}

// findingView is a finding as the merchant API answers it, with its parsed
// recommendation.
func findingView(rec reconcile.FindingRecord) billing.Finding {
	v := billing.Finding{
		ID: billing.FindingID(rec.ID), Type: string(rec.Type), SubjectKey: rec.SubjectKey, Severity: string(rec.Severity),
		Status: billing.FindingStatus(rec.Status), Evidence: rec.Evidence, LastSeenAt: rec.LastSeenAt, ResolvedAt: rec.ResolvedAt,
		Provider: optional(string(rec.Provider)), RecommendedAction: optional(rec.RecommendedAction), Resolution: optional(rec.Resolution),
		ResolvedBy: optional(rec.ResolvedBy), Notes: optional(rec.Notes), CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
	if v.Evidence == nil {
		v.Evidence = map[string]any{}
	}
	if parsed, ok := recommend.FromEvidence(rec.Evidence); ok {
		out := recommendationView(parsed)
		v.Recommendation = &out
	}
	return v
}

func recommendationView(r recommend.Recommendation) billing.FindingRecommendation {
	out := billing.FindingRecommendation{Action: r.Action, Params: r.Params}
	for _, alt := range r.Alternatives {
		out.Alternatives = append(out.Alternatives, recommendationView(alt))
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// FindingsQuery is the findings list's filters.
type FindingsQuery struct {
	Status   string `form:"status"`
	Severity string `form:"severity"`
	Type     string `form:"finding_type"`
}

// AdminListFindings handles GET /v1/merchant/findings: the operator work
// list, open findings by default, most severe first, then oldest.
func AdminListFindings(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	var q FindingsQuery
	if !r.BindQuery(&q) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	items, err := store.ListQueueFindings(r.Request.Context(), reconcile.QueueFilter{
		Severity: strings.TrimSpace(q.Severity), Type: strings.TrimSpace(q.Type), Status: strings.TrimSpace(q.Status), Page: page,
	})
	if err != nil {
		writeRefusal(r, err, "list findings failed")
		return
	}
	r.SuccessJSON(pagination.Map(items, findingView))
}

// GetFindingSummary handles GET /v1/merchant/findings/summary: the queue at
// a glance (#690). OrphanedMembers, Freeloaders and DuplicateCoverage are
// error metrics, nonzero for a full sweep (15 min) means the billing state
// machine is failing; VerificationPressure may be nonzero, but its age
// trending up means verification (pull, probe, converge) is down.
func GetFindingSummary(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	summary, err := store.Gauges(r.Request.Context())
	if err != nil {
		r.InternalError("compute finding summary failed", err)
		return
	}
	r.SuccessJSON(summary)
}

// AdminGetFinding handles GET /v1/merchant/findings/{id}.
func AdminGetFinding(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	id, ok := pathID(r, billing.ParseFindingID)
	if !ok {
		return
	}
	finding, err := store.GetFinding(r.Request.Context(), id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode(billing.CodeResourceNotFound, "")
		return
	}
	if err != nil {
		r.InternalError("load finding failed", err)
		return
	}
	r.SuccessJSON(findingView(finding))
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

// AdminResolveFinding resolves one finding: approve runs its recommendation
// (params merged with override_params) and marks it fixed; ignore silences
// the subject for good (notes required). A failed run leaves the finding open
// with the error in its notes; the next sweep re-measures, so a fix that did
// not take reopens by re-detection.
//
//	POST /v1/merchant/findings/{id}/resolve
func AdminResolveFinding(r *httprequest.Request) {
	store, ok := findingsStore(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	id, ok := pathID(r, billing.ParseFindingID)
	if !ok {
		return
	}
	var req billing.ResolveFindingParams
	if !r.BindJSON(&req) {
		return
	}
	outcome := billing.FindingOutcome(strings.ToLower(strings.TrimSpace(string(req.Outcome))))
	if outcome != billing.FindingApprove && outcome != billing.FindingIgnore {
		r.APIError(api.Coded(billing.CodeInvalidParam, `outcome must be "approve" or "ignore"`).WithParam("outcome"))
		return
	}
	notes := strings.TrimSpace(req.Notes)

	finding, err := store.GetFinding(ctx, id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode(billing.CodeResourceNotFound, "")
		return
	}
	if err != nil {
		r.InternalError("load finding failed", err)
		return
	}
	if !findingIsOpen(finding) {
		r.ErrorCode(billing.CodeResourceConflict, "finding already resolved (status="+string(finding.Status)+", resolution="+finding.Resolution+")")
		return
	}
	if outcome == billing.FindingIgnore && notes == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "notes are required to ignore a finding (permanent silence for this subject)").WithParam("notes"))
		return
	}
	execution, status, message := resolveFindingOutcome(r, store, finding, string(outcome), notes, req.OverrideParams)
	if status != http.StatusOK {
		r.ErrorJSON(status, message)
		return
	}
	updated, err := store.GetFinding(ctx, id.UUID())
	if err != nil {
		r.InternalError("finding resolved but failed to reload", err)
		return
	}
	if execution == nil {
		execution = map[string]any{}
	}
	r.SuccessJSON(billing.FindingResolution{Finding: findingView(updated), Execution: execution})
}

// resolveFinding resolves one open finding; see resolveFindingOutcome.
func resolveFinding(r *httprequest.Request, store *reconcile.PGStore, finding reconcile.FindingRecord, outcome, notes string, overrideParams json.RawMessage) (int, string) {
	_, status, message := resolveFindingOutcome(r, store, finding, outcome, notes, overrideParams)
	return status, message
}

// resolveFindingOutcome ignores (notes required) or approves one open finding,
// returning the completed execution effects or an HTTP status and message.
func resolveFindingOutcome(r *httprequest.Request, store *reconcile.PGStore, finding reconcile.FindingRecord, outcome, notes string, overrideParams json.RawMessage) (map[string]any, int, string) {
	ctx := r.Request.Context()
	id := finding.ID
	actor := resolveActorIdentity(r)
	if outcome == "ignore" {
		okRow, err := store.IgnoreFindingWithActor(ctx, id, notes, actor)
		if err != nil {
			return nil, http.StatusInternalServerError, "failed to ignore finding"
		}
		if !okRow {
			return nil, http.StatusConflict, "finding is no longer open"
		}
		return nil, http.StatusOK, ""
	}

	// approve: mechanical execution requires a structured recommendation.
	rec, hasRec := recommend.FromEvidence(finding.Evidence)
	if !hasRec {
		return nil, http.StatusUnprocessableEntity,
			"finding carries no structured recommendation; approve is unavailable — resolve with outcome=ignore or fix out-of-band"
	}
	overrides, err := recommend.DecodeParams(overrideParams)
	if err != nil {
		return nil, http.StatusBadRequest, "invalid override_params: " + err.Error()
	}
	params, err := recommend.ApplyOverrides(rec.Params, overrides)
	if err != nil {
		return nil, http.StatusBadRequest, "invalid override_params: " + err.Error()
	}
	execution, execErr := executeFindingAction(r, finding, rec.Action, params, notes)
	if execErr != nil {
		// PARTIAL FAILURE: the finding stays OPEN; the error (plus whatever
		// completed — the compensation state) is appended to operator notes.
		note := "approve by " + actor + " failed: " + execErr.Error()
		if len(execution) > 0 {
			note += " (completed: " + compactJSON(execution) + ")"
		}
		if nerr := store.AppendFindingNotes(ctx, id, note); nerr != nil {
			return nil, http.StatusInternalServerError, "execution failed and the failure note could not be recorded: " + execErr.Error()
		}
		return nil, findingActionErrorStatus(execErr), "recommendation execution failed; finding remains open: " + execErr.Error()
	}
	okRow, err := store.ResolveFindingFixed(ctx, id, notes, actor, execution)
	if err != nil {
		return nil, http.StatusInternalServerError, "recommendation executed but finding could not be marked fixed"
	}
	if !okRow {
		return nil, http.StatusConflict, "recommendation executed but the finding was no longer open"
	}
	return execution, http.StatusOK, ""
}
