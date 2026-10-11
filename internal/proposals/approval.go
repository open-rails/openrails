package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
)

// approval is one approve call: its notes, who approved, and the approval run
// it opens before its first change applies.
type approval struct {
	e        *Executor
	mid      uuid.UUID
	notes    string
	actor    string
	ids      int
	run      uuid.UUID
	approved int
	affected map[string]int
}

// errNeedRun stops an id that is about to apply until the call's run exists:
// the run commits on its own, so a failed id never takes it down.
var errNeedRun = errors.New("approval run not open")

// superseded is a basis or evidence check that failed.
type superseded struct{ reason string }

func (s superseded) Error() string { return s.reason }

func (a *approval) one(ctx context.Context, id uuid.UUID) billing.ProposalResult {
	result, err := a.try(ctx, id)
	if errors.Is(err, errNeedRun) {
		if err = a.open(ctx); err == nil {
			result, err = a.try(ctx, id)
		}
	}
	if err != nil {
		result = billing.ProposalResult{ID: billing.FindingID(id), Outcome: billing.ProposalFailed,
			Error: &billing.ProposalError{Code: "finding_action_failed", Message: err.Error()}}
		note := "approve by " + a.actor + " failed: " + err.Error()
		if nerr := a.e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := gen.New(tx).AppendReconciliationFindingNotes(ctx, gen.AppendReconciliationFindingNotesParams{ID: id, Note: note})
			return err
		}); nerr != nil {
			log.WithContext(ctx).WithError(nerr).WithField("finding_id", id).Error("proposals: the failure note could not be recorded")
		}
	}
	a.e.attachFinding(ctx, a.mid, &result)
	return result
}

// try runs one id in its own transaction.
func (a *approval) try(ctx context.Context, id uuid.UUID) (billing.ProposalResult, error) {
	result := billing.ProposalResult{ID: billing.FindingID(id)}
	err := a.e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := a.e.DB.NewWithPgxTx(tx)
		q := gen.New(tx)
		row, err := q.LockProposal(ctx, gen.LockProposalParams{MerchantID: a.mid, ID: id})
		if err != nil {
			return err
		}
		if !open(row.Status) {
			result.Outcome = billing.ProposalAlreadyResolved
			return nil
		}
		rec := reconcile.FindingRecordFromRow(row)
		if rec.Change == nil || rec.PSPID == nil {
			result.Outcome = billing.ProposalFailed
			result.Error = &billing.ProposalError{Code: "finding_not_actionable", Message: "this finding proposes no change; dismiss it"}
			return nil
		}
		now := a.e.now()
		x := &execution{e: a.e, d: txdb, q: q, mid: a.mid, psp: *rec.PSPID, finding: rec, now: now}
		if err := x.basis(ctx); err != nil {
			var s superseded
			if !errors.As(err, &s) {
				return err
			}
			if err := resolve(ctx, q, a.mid, id, "superseded", a.notes, a.actor, nil, map[string]any{"superseded": s.reason}, now); err != nil {
				return err
			}
			result.Outcome = billing.ProposalSuperseded
			return nil
		}
		if hold := a.e.holdFor(ctx, txdb, a.mid, rec.Change); hold != nil {
			result.Outcome, result.Error = billing.ProposalHeld, hold
			return nil
		}
		if a.run == uuid.Nil {
			return errNeedRun
		}
		x.run = a.run
		// The change applies in a savepoint: a write that finds nothing to do
		// leaves no before-image behind.
		var done map[string]any
		err = txdb.MerchantTx(ctx, func(ctx context.Context, sp pgx.Tx) error {
			x.d, x.q = txdb.NewWithPgxTx(sp), gen.New(sp)
			var err error
			done, err = x.apply(ctx)
			return err
		})
		x.d, x.q = txdb, q
		if err != nil {
			var s superseded
			if !errors.As(err, &s) {
				return err
			}
			if err := resolve(ctx, q, a.mid, id, "superseded", a.notes, a.actor, nil, map[string]any{"superseded": s.reason}, now); err != nil {
				return err
			}
			result.Outcome = billing.ProposalSuperseded
			return nil
		}
		done["kind"] = string(rec.Change.Kind)
		done["applied_at"] = now.Format(time.RFC3339)
		run := a.run
		if err := resolve(ctx, q, a.mid, id, "approved", a.notes, a.actor, &run, done, now); err != nil {
			return err
		}
		result.Outcome, result.Execution = billing.ProposalApproved, done
		for table, n := range x.affected {
			if a.affected == nil {
				a.affected = map[string]int{}
			}
			a.affected[table] += n
		}
		a.approved++
		return nil
	})
	return result, err
}

// open records the call's approval run before its first change applies.
func (a *approval) open(ctx context.Context) error {
	run, err := (&reconcile.PGDestructiveRunRecorder{DB: a.e.DB}).Open(ctx, reconcile.OpenDestructiveRunParams{
		Kind: reconcile.DestructiveRunKindApproval, Actor: a.actor, ExpectedRows: a.ids,
		Note: fmt.Sprintf("approval of %d proposal(s)", a.ids),
	})
	if err != nil {
		return err
	}
	a.run = run
	return nil
}

func (a *approval) finish(ctx context.Context) error {
	affected := map[string]int{"proposals": a.approved}
	for table, n := range a.affected {
		affected[table] = n
	}
	return (&reconcile.PGDestructiveRunRecorder{DB: a.e.DB}).Finish(ctx, a.run, "completed", affected)
}

func marshal(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"encode_error": err.Error()})
	}
	return b
}

// execution applies one change inside its id's transaction.
type execution struct {
	e        *Executor
	d        *db.DB
	q        *gen.Queries
	mid      uuid.UUID
	psp      uuid.UUID
	finding  reconcile.FindingRecord
	now      time.Time
	run      uuid.UUID
	affected map[string]int
}

func (x *execution) change() *reconcile.Change { return x.finding.Change }

// basis checks the change still applies: its evidence is fresh enough for an
// end of access, and the state it was computed against has not moved.
func (x *execution) basis(ctx context.Context) error {
	c := x.change()
	if c.Effects.EndsAccess && x.now.Sub(c.ObservedAt) > reconcile.EvidenceStaleAfter {
		return superseded{"evidence_stale: observed " + c.ObservedAt.Format(time.RFC3339) + ", more than 7 days ago"}
	}
	b := c.Basis
	if b.RowVersion != nil {
		subID, err := c.BasisSubscription()
		if err != nil {
			return err
		}
		sub, err := lockSubscription(ctx, x.d, subID)
		if err != nil {
			return err
		}
		if sub.RowVersion != *b.RowVersion {
			return superseded{fmt.Sprintf("subscription_moved: row version %d, the proposal was computed at %d", sub.RowVersion, *b.RowVersion)}
		}
	}
	if b.AbsentRailID != nil {
		recorded, err := x.q.ProposalRailIDRecorded(ctx, gen.ProposalRailIDRecordedParams{
			Subscription: c.Kind == billing.ChangeCreateSubscription, MerchantID: x.mid, PspID: x.psp, RailID: *b.AbsentRailID,
		})
		if err != nil {
			return err
		}
		if recorded {
			return superseded{"already_recorded: " + *b.AbsentRailID + " is recorded"}
		}
	}
	if b.CardVersionID != nil && c.Apply.AdoptPaymentMethod != nil {
		latest, err := x.q.LatestCardVersionID(ctx, gen.LatestCardVersionIDParams{MerchantID: x.mid, PaymentMethodID: c.Apply.AdoptPaymentMethod.PaymentMethodID})
		if err != nil {
			return err
		}
		current := ""
		if latest != uuid.Nil {
			current = latest.String()
		}
		if current != *b.CardVersionID {
			return superseded{"card_moved: the card has a newer version than the proposal was computed against"}
		}
	}
	return nil
}
