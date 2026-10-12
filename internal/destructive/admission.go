package destructive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Admissions owns the durable admission boundary. The root pool is retained
// independently of domain transaction rebindings; no provider call runs here.
type Admissions struct {
	db      *db.DB
	clock   clockwork.Clock
	project HoldProjector
}

func NewAdmissions(database *db.DB, clock clockwork.Clock, project HoldProjector) *Admissions {
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	return &Admissions{db: database, clock: clock, project: project}
}
func (a *Admissions) transaction(ctx context.Context, merchantID uuid.UUID, f func(context.Context, *gen.Queries, pgx.Tx) error) error {
	if a == nil || a.db == nil || a.project == nil {
		return errors.New("destructive admission and incident projector must be configured")
	}
	return a.db.DataPool().CommittedMerchantTx(ctx, billing.MerchantID(merchantID), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockDestructiveAdmissions(ctx); err != nil {
			return err
		}
		return f(ctx, q, tx)
	})
}
func scopeKeys(actor Actor, mid uuid.UUID) []string {
	return []string{"actor:" + actor.key(), "merchant:" + mid.String(), "deployment"}
}

// Admit reserves the entire validated command. A trip is committed before
// HeldError escapes. A proven no-effect retry creates a fresh immutable attempt.
func (a *Admissions) Admit(ctx context.Context, c Command) (Action, error) {
	actions, err := a.AdmitBatch(ctx, []Command{c})
	if err != nil {
		return Action{}, err
	}
	return actions[0], nil
}

// AdmitBatch atomically reserves all fresh commands of one admitted principal
// and merchant. Existing replays cost zero; no fresh receipt survives a trip.
func (a *Admissions) AdmitBatch(ctx context.Context, commands []Command) ([]Action, error) {
	if len(commands) < 1 || len(commands) > 1000 {
		return nil, errors.New("admit 1 to 1000 destructive commands")
	}
	first := commands[0]
	keysSeen := map[string]bool{}
	for _, c := range commands {
		if err := c.validate(); err != nil {
			return nil, err
		}
		if c.MerchantID != first.MerchantID || c.Actor.Issuer != first.Actor.Issuer || c.Actor.ID != first.Actor.ID {
			return nil, errors.New("destructive batch must share merchant and actor")
		}
		if keysSeen[c.Key] {
			return nil, errors.New("duplicate destructive command key")
		}
		keysSeen[c.Key] = true
	}
	out := make([]Action, len(commands))
	var held *HeldError
	err := a.transaction(ctx, first.MerchantID, func(ctx context.Context, q *gen.Queries, tx pgx.Tx) error {
		var fresh []int
		var units int64
		for i, c := range commands {
			old, err := q.GetDestructiveActionByKey(ctx, gen.GetDestructiveActionByKeyParams{MerchantID: c.MerchantID, ActionKey: c.Key})
			if err == nil {
				if old.RequestDigest != c.Digest || old.Kind != string(c.Kind) || old.Target != c.Target || old.Units != c.Units || old.ActorIssuer != c.Actor.Issuer || old.ActorID != c.Actor.ID {
					return ErrConflict
				}
				if old.State != string(StateNoEffect) {
					out[i] = actionOf(old)
					out[i].Replayed = true
					continue
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			fresh = append(fresh, i)
			units += c.Units
		}
		if len(fresh) == 0 {
			return nil
		}
		trigger := commands[fresh[0]]
		var err error
		held, err = a.admitUnits(ctx, q, tx, trigger, units)
		if err != nil || held != nil {
			return err
		}
		now := a.clock.Now().UTC()
		for _, i := range fresh {
			c := commands[i]
			origin := "staff"
			if c.Actor.Issuer == billingauth.HostIssuer {
				origin = "host"
			}
			row, err := q.CreateDestructiveAction(ctx, gen.CreateDestructiveActionParams{MerchantID: c.MerchantID, ID: uuid.New(), Kind: string(c.Kind), Target: c.Target, ActionKey: c.Key, RequestDigest: c.Digest, Origin: origin, ActorIssuer: c.Actor.Issuer, ActorID: c.Actor.ID, ActorKey: c.Actor.key(), Credential: optional(c.Actor.Credential), Units: c.Units, AdmittedAt: now})
			if err != nil {
				return err
			}
			out[i] = actionOf(row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if held != nil {
		return nil, held
	}
	return out, nil
}
func (a *Admissions) admitUnits(ctx context.Context, q *gen.Queries, tx pgx.Tx, c Command, units int64) (*HeldError, error) {
	keys := scopeKeys(c.Actor, c.MerchantID)
	if h, err := q.FindOpenDestructiveHold(ctx, keys); err == nil {
		return &HeldError{Hold: holdOf(h)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	now := a.clock.Now().UTC()
	scale, err := q.DestructiveActiveCustomerScale(ctx, gen.DestructiveActiveCustomerScaleParams{MerchantID: c.MerchantID, Since: now.Add(-30 * 24 * time.Hour)})
	if err != nil {
		return nil, err
	}
	for i, scope := range []Scope{ScopeActor, ScopeMerchant, ScopeDeployment} {
		after, err := q.DestructiveScopeClearEpoch(ctx, keys[i])
		if err != nil {
			return nil, err
		}
		var hourly, daily int64
		hourSince, daySince := now.Add(-time.Hour), now.Add(-24*time.Hour)
		switch scope {
		case ScopeActor:
			n, err := q.CountDestructiveUnitsForActor(ctx, gen.CountDestructiveUnitsForActorParams{ActorKey: c.Actor.key(), HourSince: hourSince, DaySince: daySince, ClearAfter: after})
			if err != nil {
				return nil, err
			}
			hourly, daily = n.Hourly, n.Daily
		case ScopeMerchant:
			n, err := q.CountDestructiveUnitsForMerchant(ctx, gen.CountDestructiveUnitsForMerchantParams{MerchantID: c.MerchantID, HourSince: hourSince, DaySince: daySince, ClearAfter: after})
			if err != nil {
				return nil, err
			}
			hourly, daily = n.Hourly, n.Daily
		case ScopeDeployment:
			n, err := q.CountDestructiveUnitsForDeployment(ctx, gen.CountDestructiveUnitsForDeploymentParams{HourSince: hourSince, DaySince: daySince, ClearAfter: after})
			if err != nil {
				return nil, err
			}
			hourly, daily = n.Hourly, n.Daily
		}
		hourLimit, dayLimit := limits(scope, scale.MerchantActive, scale.DeploymentActive)
		window, count, limit := "hour", hourly, hourLimit
		if hourly+units <= hourLimit {
			window, count, limit = "day", daily, dayLimit
		}
		if count+units <= limit {
			continue
		}
		row, err := q.CreateDestructiveHold(ctx, gen.CreateDestructiveHoldParams{MerchantID: c.MerchantID, ID: uuid.New(), Scope: string(scope), ScopeKey: keys[i], ActorIssuer: c.Actor.Issuer, ActorID: c.Actor.ID, Credential: optional(c.Actor.Credential), ActionKind: string(c.Kind), Target: c.Target, WindowName: window, Observed: count, Requested: units, Ceiling: limit, OpenedAt: now})
		if err != nil {
			return nil, err
		}
		h := holdOf(row)
		if err := a.project(ctx, tx, h); err != nil {
			return nil, fmt.Errorf("project destructive incident: %w", err)
		}
		return &HeldError{Hold: h}, nil
	}
	return nil, nil
}

// Start is the first-effect fence. Once a local or provider effect started,
// recovery and required consequences keep that admission through later holds.
func (a *Admissions) Start(ctx context.Context, mid, id uuid.UUID) (Action, error) {
	var out Action
	var held *HeldError
	err := a.transaction(ctx, mid, func(ctx context.Context, q *gen.Queries, _ pgx.Tx) error {
		row, err := q.LockDestructiveAction(ctx, gen.LockDestructiveActionParams{MerchantID: mid, ID: id})
		if err != nil {
			return err
		}
		if row.State == string(StateNoEffect) {
			return ErrActionFinal
		}
		if row.State != string(StateReserved) {
			out = actionOf(row)
			out.Replayed = true
			return nil
		}
		if h, err := q.FindOpenDestructiveHold(ctx, scopeKeys(Actor{Issuer: row.ActorIssuer, ID: row.ActorID}, mid)); err == nil {
			held = &HeldError{Hold: holdOf(h)}
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err = q.StartDestructiveAction(ctx, gen.StartDestructiveActionParams{MerchantID: mid, ID: id, Now: a.clock.Now().UTC()})
		if err == nil {
			out = actionOf(row)
		}
		return err
	})
	if err != nil {
		return Action{}, err
	}
	if held != nil {
		return Action{}, held
	}
	return out, nil
}

// Finish records only a proven outcome; a generic network error is Uncertain.
func (a *Admissions) Finish(ctx context.Context, mid, id uuid.UUID, outcome Outcome, result string) error {
	return a.transaction(ctx, mid, func(ctx context.Context, _ *gen.Queries, tx pgx.Tx) error {
		return a.FinishTx(ctx, tx, mid, id, outcome, result)
	})
}

// FinishTx couples a local effect and its outcome to the caller's transaction.
// Provider writers call it after updating the intent and enqueueing consequences.
func (a *Admissions) FinishTx(ctx context.Context, tx pgx.Tx, mid, id uuid.UUID, outcome Outcome, result string) error {
	if a == nil || tx == nil || mid == uuid.Nil || id == uuid.Nil {
		return errors.New("destructive outcome has no admission")
	}
	if len(result) > 2048 {
		return errors.New("destructive result reference too long")
	}
	switch outcome {
	case OutcomeSucceeded, OutcomeNoEffect, OutcomeUncertain:
	default:
		return errors.New("invalid destructive outcome")
	}
	q := gen.New(tx)
	row, err := q.LockDestructiveAction(ctx, gen.LockDestructiveActionParams{MerchantID: mid, ID: id})
	if err != nil {
		return err
	}
	if row.State == string(StateSucceeded) || row.State == string(StateNoEffect) {
		if row.State == string(outcome) || row.State == string(StateSucceeded) && outcome == OutcomeNoEffect {
			return nil
		}
		return ErrActionFinal
	}
	provider, err := q.DestructiveActionProviderState(ctx, gen.DestructiveActionProviderStateParams{MerchantID: mid, ID: id})
	if err != nil {
		return err
	}
	if outcome == OutcomeNoEffect && (provider.Pending || provider.Attempted) {
		return errors.New("provider effects cannot be declared absent")
	}
	if outcome == OutcomeSucceeded && row.State == string(StateReserved) {
		return errors.New("destructive action has not started")
	}
	now := a.clock.Now().UTC()
	state := string(outcome)
	started := row.StartedAt
	var finished *time.Time
	if outcome == OutcomeSucceeded && provider.Pending {
		state = string(StateStarted)
	} else if outcome != OutcomeUncertain {
		finished = &now
	}
	if outcome == OutcomeUncertain && started == nil {
		started = &now
	}
	_, err = q.FinishDestructiveAction(ctx, gen.FinishDestructiveActionParams{MerchantID: mid, ID: id, State: state, StartedAt: started, FinishedAt: finished, Result: optional(result)})
	return err
}

func (a *Admissions) GetAction(ctx context.Context, mid, id uuid.UUID) (Action, error) {
	var out Action
	if a == nil || a.db == nil {
		return out, errors.New("destructive admissions unavailable")
	}
	err := a.db.RunInMerchantConn(merchant.WithID(ctx, billing.MerchantID(mid)), func(ctx context.Context) error {
		row, err := a.db.Gen(ctx).GetDestructiveAction(ctx, gen.GetDestructiveActionParams{MerchantID: mid, ID: id})
		if err == nil {
			out = actionOf(row)
		}
		return err
	})
	return out, err
}
func limits(scope Scope, merchant, all int64) (int64, int64) {
	switch scope {
	case ScopeActor:
		return 5, 15
	case ScopeMerchant:
		return max(int64(15), ceil(merchant, 400)), max(int64(25), ceil(merchant, 100))
	default:
		return max(int64(50), ceil(all, 400)), max(int64(100), ceil(all, 100))
	}
}
func ceil(n, d int64) int64 { return n/d + min(n%d, 1) }
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func actionOf(r gen.BillingDestructiveAction) Action {
	return Action{ID: r.ID, MerchantID: r.MerchantID, Kind: Kind(r.Kind), Target: r.Target, Key: r.ActionKey, Digest: r.RequestDigest, Units: r.Units, Actor: Actor{Issuer: r.ActorIssuer, ID: r.ActorID, Credential: text(r.Credential)}, State: State(r.State), AdmittedAt: r.AdmittedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Result: text(r.Result)}
}

// Clear is called only after the boundary proves root human authority, or by
// explicitly trusted operator code. It still refuses the triggering principal.
func (a *Admissions) Clear(ctx context.Context, mid, id uuid.UUID, by Actor, reason string) (Hold, error) {
	if err := by.validate(); err != nil {
		return Hold{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 {
		return Hold{}, errors.New("clear reason is required (at most 2000 bytes)")
	}
	var out Hold
	err := a.transaction(ctx, mid, func(ctx context.Context, q *gen.Queries, tx pgx.Tx) error {
		row, err := q.GetDestructiveHold(ctx, gen.GetDestructiveHoldParams{MerchantID: mid, ID: id})
		if err != nil {
			return err
		}
		if row.ActorIssuer == by.Issuer && row.ActorID == by.ID {
			return ErrSelfClear
		}
		if row.ClearedAt != nil {
			if text(row.ClearedByIssuer) != by.Issuer || text(row.ClearedByID) != by.ID || text(row.ClearReason) != reason {
				return ErrConflict
			}
			out = holdOf(row)
			return nil
		}
		row, err = q.ClearDestructiveHold(ctx, gen.ClearDestructiveHoldParams{MerchantID: mid, ID: id, Issuer: by.Issuer, ActorID: by.ID, Reason: reason, Now: a.clock.Now().UTC()})
		if err != nil {
			return err
		}
		out = holdOf(row)
		if err := a.project(ctx, tx, out); err != nil {
			return fmt.Errorf("project destructive clear: %w", err)
		}
		return nil
	})
	return out, err
}
func holdOf(r gen.BillingDestructiveHold) Hold {
	h := Hold{ID: r.ID, MerchantID: r.MerchantID, Scope: Scope(r.Scope), ScopeKey: r.ScopeKey, TriggerActor: Actor{Issuer: r.ActorIssuer, ID: r.ActorID, Credential: text(r.Credential)}, ActionKind: Kind(r.ActionKind), Target: r.Target, Window: r.WindowName, Observed: r.Observed, Requested: r.Requested, Limit: r.Ceiling, OpenedAt: r.OpenedAt, ClearedAt: r.ClearedAt, ClearReason: text(r.ClearReason)}
	if r.ClearedAt != nil {
		h.ClearedBy = &Actor{Issuer: text(r.ClearedByIssuer), ID: text(r.ClearedByID)}
	}
	return h
}
func (a *Admissions) GetHold(ctx context.Context, mid, id uuid.UUID) (Hold, error) {
	var out Hold
	if a == nil || a.db == nil {
		return out, errors.New("destructive admissions unavailable")
	}
	err := a.db.RunInMerchantConn(merchant.WithID(ctx, billing.MerchantID(mid)), func(ctx context.Context) error {
		row, err := a.db.Gen(ctx).GetDestructiveHold(ctx, gen.GetDestructiveHoldParams{MerchantID: mid, ID: id})
		if err == nil {
			out = holdOf(row)
		}
		return err
	})
	return out, err
}
func (a *Admissions) ListHolds(ctx context.Context, mid uuid.UUID, limit int, after *uuid.UUID) ([]Hold, error) {
	if limit < 1 || limit > 101 {
		return nil, errors.New("fetch 1 to 101 holds")
	}
	out := []Hold{}
	if a == nil || a.db == nil {
		return nil, errors.New("destructive admissions unavailable")
	}
	err := a.db.RunInMerchantConn(merchant.WithID(ctx, billing.MerchantID(mid)), func(ctx context.Context) error {
		rows, err := a.db.Gen(ctx).ListDestructiveHolds(ctx, gen.ListDestructiveHoldsParams{MerchantID: mid, AfterID: after, RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, holdOf(r))
		}
		return nil
	})
	return out, err
}

type HoldAddress struct{ MerchantID, ID uuid.UUID }

func (a *Admissions) ListOpenHoldsDirectory(ctx context.Context, limit int, after *uuid.UUID) ([]HoldAddress, error) {
	if limit < 1 || limit > 101 {
		return nil, errors.New("fetch 1 to 101 holds")
	}
	if a == nil || a.db == nil {
		return nil, errors.New("destructive admissions unavailable")
	}
	rows, err := a.db.GenDirectory().ListOpenDestructiveHoldAddresses(ctx, gen.ListOpenDestructiveHoldAddressesParams{AfterID: after, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]HoldAddress, 0, len(rows))
	for _, r := range rows {
		out = append(out, HoldAddress{r.MerchantID, r.ID})
	}
	return out, nil
}
