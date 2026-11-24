package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A declared book is one command, and every destructive effect it lands is
// admitted with it in its actor's name: a cancel, and the provider stop owed
// a canceled row. A cancel of a row the book did not create also passes the
// destructive switch and its PSP's cancel budget, all or nothing per PSP; a
// held one parks the row unverified with access kept. A book is landed, then
// admitted, then landed again: a landing that wants an effect it holds no
// admission for rolls back with ErrDeclaredAdmission.

// ErrDeclaredAdmission rolls back a landing of a book that wants destructive
// effects: Admit them, then land the book again.
var ErrDeclaredAdmission = errors.New("declared book: its destructive effects need admission")

// DeclaredBook is one book's destructive effects across its landings.
type DeclaredBook struct {
	admissions *destructive.Admissions
	actor      *destructive.Actor
	asOf       time.Time

	admitted bool
	actions  map[declaredRef]destructive.Action
	held     map[declaredRef]string
	started  map[uuid.UUID]bool

	// The current landing's.
	wanted   map[declaredRef]declaredEffect
	created  map[declaredRef]bool
	finished map[uuid.UUID]bool
}

// NewDeclaredBook is a book landed at asOf by actor (nil: the context's
// admitted actor).
func NewDeclaredBook(admissions *destructive.Admissions, actor *destructive.Actor, asOf time.Time) *DeclaredBook {
	return &DeclaredBook{admissions: admissions, actor: actor, asOf: asOf.UTC()}
}

// declaredRef is a book row by its provider identity, the same in every
// landing.
type declaredRef struct {
	psp       uuid.UUID
	rail, ref string
}

func refOf(sub *models.Subscription) declaredRef {
	return declaredRef{psp: sub.PspID, rail: string(sub.Rail), ref: sub.RailSubscriptionID}
}

func (r declaredRef) target() string {
	return billing.PSPID(r.psp).String() + "/" + r.rail + "/" + r.ref
}

func (r declaredRef) compare(o declaredRef) int {
	return cmp.Or(strings.Compare(r.psp.String(), o.psp.String()), strings.Compare(r.rail, o.rail), strings.Compare(r.ref, o.ref))
}

type declaredEffect struct {
	// existing: a cancel of a row the book did not create.
	existing bool
}

type declaredBookKey struct{}

func withDeclaredBook(ctx context.Context, b *DeclaredBook) context.Context {
	return context.WithValue(ctx, declaredBookKey{}, b)
}

func declaredBookOf(ctx context.Context) *DeclaredBook {
	b, _ := ctx.Value(declaredBookKey{}).(*DeclaredBook)
	return b
}

// begin starts a landing.
func (b *DeclaredBook) begin() {
	b.wanted, b.created, b.finished = map[declaredRef]declaredEffect{}, map[declaredRef]bool{}, map[uuid.UUID]bool{}
}

// create records a row this landing created.
func (b *DeclaredBook) create(sub *models.Subscription) { b.created[refOf(sub)] = true }

// bookMark is a landing's state before a savepoint, restored when it rolls
// back.
type bookMark struct {
	wanted   map[declaredRef]declaredEffect
	finished map[uuid.UUID]bool
}

func (b *DeclaredBook) mark() bookMark {
	return bookMark{wanted: maps.Clone(b.wanted), finished: maps.Clone(b.finished)}
}

func (b *DeclaredBook) reset(m bookMark) { b.wanted, b.finished = m.wanted, m.finished }

// needsAdmission reports a landing that wanted effects before any admission.
func (b *DeclaredBook) needsAdmission() bool { return !b.admitted && len(b.wanted) > 0 }

// effect returns the context an admitted effect on sub runs in, or why it is
// held. Before admission it records the effect and holds it.
func (b *DeclaredBook) effect(ctx context.Context, database *db.DB, sub *models.Subscription, cancel bool) (context.Context, string) {
	ref := refOf(sub)
	existing := cancel && !b.created[ref]
	if !b.admitted {
		b.wanted[ref] = declaredEffect{existing: existing}
		return ctx, "awaiting the book's admission"
	}
	if hold := b.held[ref]; hold != "" {
		return ctx, hold
	}
	action, ok := b.actions[ref]
	if !ok {
		return ctx, "the book was admitted without this effect"
	}
	if existing {
		if v := destructive.New(database).Check(ctx, sub.MerchantID); !v.Allowed {
			return ctx, v.Reason
		}
	}
	return destructive.BindAction(destructive.BindAdmissions(ctx, b.admissions), action), ""
}

// cancel applies a declared cancel of sub when the book's admission allows
// it; a held one parks the live row unverified.
func (b *DeclaredBook) cancel(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision, now time.Time) (bool, error) {
	ctx, hold := b.effect(ctx, database, sub, true)
	if hold != "" {
		if b.admitted {
			database.Log.WarnContext(ctx, "declared cancel held; the subscription keeps its access", "subscription_id", sub.ID, "rail", sub.Rail, "reason", d.Reason, "held_by", hold)
		}
		if !b.admitted || sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
			return false, nil
		}
		return true, lc.ApplyLocalUnknown(ctx, database, sub)
	}
	applied, err := transition(ctx, database, lc, sub, d, now)
	if err != nil || !applied || sub.Status != models.StatusCanceled {
		return applied, err
	}
	return true, b.finish(ctx, database)
}

// stopProvider schedules the provider stop a canceled row is owed, once
// admitted.
func (b *DeclaredBook) stopProvider(ctx context.Context, database *db.DB, sub *models.Subscription, schedule func(context.Context) error) error {
	ctx, hold := b.effect(ctx, database, sub, false)
	switch {
	case !b.admitted:
		return nil
	case hold != "":
		return fmt.Errorf("declared book: the provider stop of %s is not admitted (%s); import the book again", refOf(sub).target(), hold)
	}
	if err := schedule(ctx); err != nil {
		return err
	}
	return b.finish(ctx, database)
}

// finish records the effect of the action bound to ctx in the landing.
func (b *DeclaredBook) finish(ctx context.Context, database *db.DB) error {
	action, ok := destructive.ActionFromContext(ctx)
	if !ok || !b.started[action.ID] || b.finished[action.ID] {
		return nil
	}
	b.finished[action.ID] = true
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return b.admissions.FinishTx(ctx, tx, action.MerchantID, action.ID, destructive.OutcomeSucceeded, action.Target)
	})
}

// end closes a landing: an admitted effect it did not apply had none.
func (b *DeclaredBook) end(ctx context.Context, database *db.DB) error {
	if !b.admitted {
		return nil
	}
	for _, action := range b.actions {
		if !b.started[action.ID] || b.finished[action.ID] {
			continue
		}
		b.finished[action.ID] = true
		if err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return b.admissions.FinishTx(ctx, tx, action.MerchantID, action.ID, destructive.OutcomeNoEffect, "")
		}); err != nil {
			return err
		}
	}
	return nil
}

// Admit admits what the last landing wanted, outside any transaction. A
// denied admission is a destructive.HeldError and starts nothing.
func (b *DeclaredBook) Admit(ctx context.Context, database *db.DB, mid billing.MerchantID) error {
	b.admitted, b.actions, b.held, b.started = true, map[declaredRef]destructive.Action{}, map[declaredRef]string{}, map[uuid.UUID]bool{}
	refs := slices.SortedFunc(maps.Keys(b.wanted), declaredRef.compare)
	var admit []declaredRef
	byPSP := map[uuid.UUID][]declaredRef{}
	var gate *destructive.Verdict
	for _, ref := range refs {
		if !b.wanted[ref].existing {
			admit = append(admit, ref)
			continue
		}
		if gate == nil {
			v := destructive.New(database).CheckMerchant(ctx, mid.UUID())
			gate = &v
		}
		if !gate.Allowed {
			b.held[ref] = gate.Reason
			continue
		}
		byPSP[ref.psp] = append(byPSP[ref.psp], ref)
	}
	for _, psp := range slices.SortedFunc(maps.Keys(byPSP), func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }) {
		planned := byPSP[psp]
		err := database.MerchantTx(merchant.WithID(ctx, mid), func(ctx context.Context, tx pgx.Tx) error {
			q := database.NewWithPgxTx(tx).Gen(ctx)
			live, err := q.CountLiveLinkedSubscriptionsForPSP(ctx, gen.CountLiveLinkedSubscriptionsForPSPParams{MerchantID: mid.UUID(), PspID: psp})
			if err != nil {
				return err
			}
			exceeded, reason := (CancelBudget{}).Exceeded(len(planned), int(live))
			if !exceeded {
				admit = append(admit, planned...)
				return nil
			}
			for _, ref := range planned {
				b.held[ref] = reason
			}
			recordGuardFinding(ctx, database.Log, q, mid, ProviderDeclared, "cancellation_cap:"+psp.String(), reason)
			return nil
		})
		if err != nil {
			return fmt.Errorf("declared book: weigh cancels against PSP %s: %w", psp, err)
		}
	}
	if len(admit) == 0 {
		return nil
	}
	actor := b.actor
	if actor == nil && b.admissions != nil {
		if from, err := destructive.ActorFromContext(ctx); err == nil {
			actor = &from
		}
	}
	if b.admissions == nil || actor == nil {
		// Work no one is answerable for destroys nothing.
		for _, ref := range admit {
			b.held[ref] = "the book has no admitted actor"
		}
		return nil
	}
	actorKey := destructive.Digest([]string{actor.Issuer, actor.ID})
	commands := make([]destructive.Command, len(admit))
	for i, ref := range admit {
		target := ref.target()
		digest := destructive.Digest([]any{"billing_import", target, b.asOf})
		commands[i] = destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CancelSubscription, Target: target,
			Key: "billing-import:" + digest + ":actor:" + actorKey, Digest: digest, Units: 1, Actor: *actor}
	}
	for from := 0; from < len(commands); from += admitBatch {
		to := min(from+admitBatch, len(commands))
		actions, err := b.admissions.AdmitBatch(ctx, commands[from:to])
		if err != nil {
			return err
		}
		for i, action := range actions {
			started, err := b.admissions.Start(ctx, mid.UUID(), action.ID)
			if err != nil {
				return err
			}
			if !started.Replayed {
				b.started[started.ID] = true
			}
			b.actions[admit[from+i]] = started
		}
	}
	return nil
}

// admitBatch is the most commands one admission reserves.
const admitBatch = 1000

// Abandon records that the admitted effects of a landing that did not commit
// had none, or are uncertain when its commit may have.
func (b *DeclaredBook) Abandon(ctx context.Context, uncertain bool) error {
	outcome := destructive.OutcomeNoEffect
	if uncertain {
		outcome = destructive.OutcomeUncertain
	}
	ctx, cancel := db.DetachedWriteContext(ctx, 10*time.Second)
	defer cancel()
	var errs []error
	for _, action := range b.actions {
		if b.started[action.ID] {
			errs = append(errs, b.admissions.Finish(ctx, action.MerchantID, action.ID, outcome, ""))
		}
	}
	return errors.Join(errs...)
}
