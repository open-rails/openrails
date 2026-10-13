// Package proposals approves and dismisses the pull.* plane of the findings
// queue. It is the only code that applies a proposal's change: HTTP, the CLI
// and in-process callers all go through Executor. Each id runs in its own
// transaction; one approve call opens at most one approval run, which
// `openrails undo-run` reverses.
package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/writeposture"
)

// Executor approves, dismisses and lists proposals for the merchant on the
// context.
type Executor struct {
	DB *db.DB
	// Config is the process's: its provider_write_mode bounds the write
	// posture. Nil reads as readonly.
	Config *config.Config
	Clock  clockwork.Clock
}

// New builds an executor.
func New(database *db.DB, cfg *config.Config, clock clockwork.Clock) *Executor {
	return &Executor{DB: database, Config: cfg, Clock: timeutil.FirstClock(clock)}
}

// Request is one approve or dismiss call. Actor is who decided: the staff
// subject, "cli:<os user>" or "host".
type Request struct {
	IDs   []billing.FindingID
	Notes string
	Actor string
}

func (e *Executor) now() time.Time { return timeutil.FirstClock(e.Clock).Now().UTC() }

// Approve applies each listed proposal whose basis still holds and that
// nothing holds. An unknown id or one that is not a pull.* finding refuses the
// whole call before anything runs.
func (e *Executor) Approve(ctx context.Context, req Request) (billing.ProposalResults, error) {
	mid, err := e.validate(ctx, req, false)
	if err != nil {
		return billing.ProposalResults{}, err
	}
	call := &approval{e: e, mid: mid, notes: strings.TrimSpace(req.Notes), actor: req.Actor, ids: len(req.IDs)}
	out := billing.ProposalResults{Data: make([]billing.ProposalResult, 0, len(req.IDs))}
	for _, id := range req.IDs {
		out.Data = append(out.Data, call.one(ctx, id.UUID()))
	}
	if call.run != uuid.Nil {
		if err := call.finish(ctx); err != nil {
			return out, err
		}
		run := billing.RunID(call.run)
		out.RunID = &run
	}
	return out, nil
}

// Dismiss closes each listed open proposal without applying it; notes are
// required. A dismissed proposal is raised again only on other evidence.
func (e *Executor) Dismiss(ctx context.Context, req Request) (billing.ProposalResults, error) {
	mid, err := e.validate(ctx, req, true)
	if err != nil {
		return billing.ProposalResults{}, err
	}
	notes := strings.TrimSpace(req.Notes)
	out := billing.ProposalResults{Data: make([]billing.ProposalResult, 0, len(req.IDs))}
	for _, id := range req.IDs {
		result := billing.ProposalResult{ID: id}
		err := e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			row, err := q.LockProposal(ctx, gen.LockProposalParams{MerchantID: mid, ID: id.UUID()})
			if err != nil {
				return err
			}
			if !open(row.Status) {
				result.Outcome = billing.ProposalAlreadyResolved
				return nil
			}
			if err := resolve(ctx, q, mid, id.UUID(), "dismissed", notes, req.Actor, nil, nil, e.now()); err != nil {
				return err
			}
			result.Outcome = billing.ProposalDismissed
			return nil
		})
		if err != nil {
			return out, fmt.Errorf("dismiss %s: %w", id, err)
		}
		e.attachFinding(ctx, mid, &result)
		out.Data = append(out.Data, result)
	}
	return out, nil
}

// Foreign closes each listed open (or dismissed) proposal as a provider record that is not
// this merchant's obligation, such as another environment's schedule on a
// shared gateway; notes are required. Unlike a dismissed one it no longer
// holds provider recovery. Only a provider schedule nothing local names
// qualifies: no subscription by its id, no payment method or PSP customer by
// its customer reference, no customer its read matched. Anything else is
// refused and stays open.
func (e *Executor) Foreign(ctx context.Context, req Request) (billing.ProposalResults, error) {
	mid, err := e.validate(ctx, req, true)
	if err != nil {
		return billing.ProposalResults{}, err
	}
	notes := strings.TrimSpace(req.Notes)
	out := billing.ProposalResults{Data: make([]billing.ProposalResult, 0, len(req.IDs))}
	for _, id := range req.IDs {
		result := billing.ProposalResult{ID: id}
		err := e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			row, err := q.LockProposal(ctx, gen.LockProposalParams{MerchantID: mid, ID: id.UUID()})
			if err != nil {
				return err
			}
			dismissed := row.Status == string(reconcile.FindingStatusIgnored) && deref(row.Resolution) == "dismissed"
			if !open(row.Status) && !dismissed {
				result.Outcome = billing.ProposalAlreadyResolved
				return nil
			}
			if reason, err := notForeign(ctx, q, mid, row); err != nil || reason != "" {
				if err == nil {
					result.Outcome = billing.ProposalFailed
					result.Error = &billing.ProposalError{Code: "finding_not_actionable", Message: reason}
				}
				return err
			}
			var evidence map[string]any
			if dismissed {
				evidence = map[string]any{"previous": map[string]any{
					"resolution": "dismissed", "notes": deref(row.OperatorNotes), "resolved_by": deref(row.ResolvedBy), "resolved_at": row.ResolvedAt,
				}}
			}
			if err := resolveFrom(ctx, q, mid, id.UUID(), "foreign", notes, req.Actor, nil, evidence, e.now(), dismissed); err != nil {
				return err
			}
			result.Outcome = billing.ProposalForeign
			return nil
		})
		if err != nil {
			return out, fmt.Errorf("resolve %s as foreign: %w", id, err)
		}
		e.attachFinding(ctx, mid, &result)
		out.Data = append(out.Data, result)
	}
	return out, nil
}

// notForeign is why row cannot be foreign, or "" when it can.
func notForeign(ctx context.Context, q *gen.Queries, mid uuid.UUID, row gen.BillingReconciliationFinding) (string, error) {
	if row.FindingType != string(reconcile.FindingRemoteSubMissingLocal) || row.PspID == nil {
		return "only a provider schedule unknown locally (" + string(reconcile.FindingRemoteSubMissingLocal) + ") can be foreign; dismiss this one", nil
	}
	local, err := q.ProposalSubjectIsLocal(ctx, gen.ProposalSubjectIsLocalParams{MerchantID: mid, ID: row.ID})
	if err != nil {
		return "", err
	}
	if local {
		return "a subscription, payment method or PSP customer of this merchant names this provider record", nil
	}
	var evidence struct {
		Local struct {
			EmailCandidates []json.RawMessage `json:"email_candidates"`
		} `json:"local"`
	}
	if len(row.Evidence) > 0 {
		if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
			return "", fmt.Errorf("read finding evidence: %w", err)
		}
	}
	if len(evidence.Local.EmailCandidates) > 0 {
		return "the provider read matched this record to a customer of this merchant by email", nil
	}
	return "", nil
}

// List pages proposals: open pull.* findings unless Status is set, most
// severe first, then oldest.
func (e *Executor) List(ctx context.Context, p billing.ProposalListParams) (billing.ListPage[billing.Finding], error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.Finding]{}, err
	}
	q := e.DB.Gen(ctx)
	if p.IDs != nil {
		if len(p.IDs) == 0 || len(p.IDs) > billing.MaxBatchItems {
			return billing.ListPage[billing.Finding]{}, api.Coded(billing.CodeInvalidParam, fmt.Sprintf("ids names 1 to %d proposals", billing.MaxBatchItems)).WithParam("ids")
		}
		ids := make([]uuid.UUID, len(p.IDs))
		for i, id := range p.IDs {
			ids[i] = id.UUID()
		}
		rows, err := q.ListReconciliationFindingsByIDs(ctx, gen.ListReconciliationFindingsByIDsParams{MerchantID: mid.UUID(), Ids: ids})
		if err != nil {
			return billing.ListPage[billing.Finding]{}, err
		}
		var page billing.ListPage[billing.Finding]
		for _, row := range rows {
			if strings.HasPrefix(row.FindingType, "pull.") {
				page.Items = append(page.Items, reconcile.FindingRecordFromRow(row).View())
			}
		}
		return page, nil
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Finding]{}, err
	}
	var after position
	present, err := pagination.Decode(p.Cursor, &after)
	if err != nil {
		return billing.ListPage[billing.Finding]{}, err
	}
	params := gen.ListProposalsParams{MerchantID: mid.UUID(), RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterRank, params.AfterAt, params.AfterID = &after.Rank, &after.At, &after.ID
	}
	if p.Status != "" {
		status := string(p.Status)
		params.Status = &status
	}
	if !p.PSPID.IsZero() {
		psp := p.PSPID.UUID()
		params.PspID = &psp
	}
	if p.Kind != "" {
		kind := string(p.Kind)
		params.Kind = &kind
	}
	if p.Source != "" {
		source := string(p.Source)
		params.Source = &source
	}
	rows, err := q.ListProposals(ctx, params)
	if err != nil {
		return billing.ListPage[billing.Finding]{}, err
	}
	records := make([]reconcile.FindingRecord, len(rows))
	for i, row := range rows {
		records[i] = reconcile.FindingRecordFromRow(row)
	}
	cut := pagination.Cut(records, limit, func(f reconcile.FindingRecord) any {
		return position{Rank: severityRank(f.Severity), At: f.CreatedAt, ID: f.ID}
	})
	return pagination.Map(cut, reconcile.FindingRecord.View), nil
}

// position is the list's keyset: severity rank, then age.
type position struct {
	Rank int32     `json:"r"`
	At   time.Time `json:"t"`
	ID   uuid.UUID `json:"i"`
}

// severityRank orders the list as its query does: critical first.
func severityRank(s reconcile.Severity) int32 {
	switch s {
	case reconcile.SeverityCritical:
		return 0
	case reconcile.SeverityHigh:
		return 1
	case reconcile.SeverityMedium:
		return 2
	}
	return 3
}

// validate refuses a call that names no ids, more than MaxBatchItems, an id
// twice, an unknown id or a finding outside the pull.* plane.
func (e *Executor) validate(ctx context.Context, req Request, dismiss bool) (uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return uuid.Nil, errors.New("proposals: an approve or dismiss names who decided")
	}
	if dismiss && strings.TrimSpace(req.Notes) == "" {
		return uuid.Nil, api.Coded(billing.CodeInvalidParam, "notes are required to dismiss a proposal").WithParam("notes")
	}
	if len(req.IDs) == 0 || len(req.IDs) > billing.MaxBatchItems {
		return uuid.Nil, api.Coded(billing.CodeInvalidParam, fmt.Sprintf("ids names 1 to %d proposals", billing.MaxBatchItems)).WithParam("ids")
	}
	seen := make(map[uuid.UUID]bool, len(req.IDs))
	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, id := range req.IDs {
		if id.IsZero() || seen[id.UUID()] {
			return uuid.Nil, api.Coded(billing.CodeInvalidParam, "ids must be distinct finding ids").WithParam("ids")
		}
		seen[id.UUID()] = true
		ids = append(ids, id.UUID())
	}
	rows, err := e.DB.Gen(ctx).ListReconciliationFindingsByIDs(ctx, gen.ListReconciliationFindingsByIDsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return uuid.Nil, err
	}
	found := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		if strings.HasPrefix(row.FindingType, "pull.") {
			found[row.ID] = true
		}
	}
	for _, id := range req.IDs {
		if !found[id.UUID()] {
			return uuid.Nil, api.Coded(billing.CodeInvalidParam, id.String()+" is not a proposal (an open or resolved pull.* finding of this merchant)").WithParam("ids")
		}
	}
	return mid.UUID(), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func open(status string) bool {
	return status == string(reconcile.FindingStatusReconcileRequired) || status == string(reconcile.FindingStatusRequiresReview)
}

// resolve closes an open proposal: approved and superseded as fixed,
// dismissed as ignored.
func resolve(ctx context.Context, q *gen.Queries, mid, id uuid.UUID, resolution, notes, actor string, run *uuid.UUID, evidence map[string]any, now time.Time) error {
	return resolveFrom(ctx, q, mid, id, resolution, notes, actor, run, evidence, now, false)
}

func resolveFrom(ctx context.Context, q *gen.Queries, mid, id uuid.UUID, resolution, notes, actor string, run *uuid.UUID, evidence map[string]any, now time.Time, fromDismissed bool) error {
	var notesPtr, actorPtr *string
	if notes != "" {
		notesPtr = &notes
	}
	if actor != "" {
		actorPtr = &actor
	}
	n, err := q.ResolveProposal(ctx, gen.ResolveProposalParams{
		MerchantID: mid, ID: id, Resolution: resolution, OperatorNotes: notesPtr, ResolvedBy: actorPtr,
		ApprovalRunID: run, ResolutionEvidence: marshal(evidence), Now: now, FromDismissed: fromDismissed,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("proposal %s is no longer open", id)
	}
	return nil
}

// attachFinding reloads the finding a result reports.
func (e *Executor) attachFinding(ctx context.Context, mid uuid.UUID, r *billing.ProposalResult) {
	rows, err := e.DB.Gen(ctx).ListReconciliationFindingsByIDs(ctx, gen.ListReconciliationFindingsByIDsParams{MerchantID: mid, Ids: []uuid.UUID{r.ID.UUID()}})
	if err == nil && len(rows) == 1 {
		f := reconcile.FindingRecordFromRow(rows[0]).View()
		r.Finding = &f
	}
}

// holdFor is what holds a change now, or nil: the kill switch holds ending
// access and provider writes, a readonly posture moving money and provider
// writes.
func (e *Executor) holdFor(ctx context.Context, d *db.DB, mid uuid.UUID, c *reconcile.Change) *billing.ProposalError {
	if c.Effects.EndsAccess || c.Effects.ProviderWrite {
		if v := destructive.New(d).Check(ctx, mid); !v.Allowed {
			return &billing.ProposalError{Code: billing.CodeDestructiveActionsHeld, Message: v.Reason}
		}
	}
	if c.Effects.MovesMoney || c.Effects.ProviderWrite {
		if p := (writeposture.View{Config: e.Config, DB: d}).Posture(ctx, mid); p.ReadOnly() {
			return &billing.ProposalError{Code: billing.CodeProviderWritesHeld, Message: p.Reason}
		}
	}
	return nil
}
