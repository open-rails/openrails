package destructive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Erasures (EraseContact) keep a budget and holds of their own: the
// merchant's erasure scope (erasureKey), which counts and holds only erasures
// and is sized to its directory. No other command counts toward it or is held
// by it, and erasures count toward and are held by nothing else. An erasure is
// never refused: behind an earlier one, under a hold or over the budget it
// waits reserved, and the first over the budget opens the scope's hold. The
// queue drains within the budget (StartErasures) while the scope is unheld.

// ScopeErasure is a merchant's erasure scope.
const ScopeErasure Scope = "erasure"

// StuckErasure is how long a started erasure may run before the drain runs it
// again: an erasure is safe to repeat.
const StuckErasure = 5 * time.Minute

// The erasure budget rises above its configured floors with the merchant's
// directory: one an hour per erasureHourShare provisioned contacts, one a day
// per erasureDayShare.
const (
	erasureHourShare = 50
	erasureDayShare  = 10
)

func erasureKey(mid uuid.UUID) string { return "erasure:" + mid.String() }

// erasureTransaction runs f under the merchant's erasure lock alone: erasures
// share no budget with any other command or merchant.
func (a *Admissions) erasureTransaction(ctx context.Context, mid uuid.UUID, f func(context.Context, *gen.Queries, pgx.Tx) error) error {
	if a == nil || a.db == nil || a.project == nil {
		return errors.New("destructive admission and incident projector must be configured")
	}
	if a.policyErr != nil {
		return a.policyErr
	}
	return a.db.DataPool().CommittedMerchantTx(ctx, billing.MerchantID(mid), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockErasureAdmissions(ctx, mid); err != nil {
			return err
		}
		return f(ctx, q, tx)
	})
}

// AdmitErasure records erasure c and starts it when nothing waits before it,
// its scope is unheld and its budget has room; otherwise it waits reserved. A
// replay answers the recorded admission. The in-process host's own erasure
// starts at once, uncounted. The caller runs an erasure that answers started
// or uncertain. Either way the drain is queued with it: at once for one that
// waits, past StuckErasure for one that started, to run it again should its
// run not finish.
func (a *Admissions) AdmitErasure(ctx context.Context, c Command) (Action, error) {
	if c.Kind != EraseContact {
		return Action{}, errors.New("AdmitErasure admits erasures only")
	}
	if err := c.validate(); err != nil {
		return Action{}, err
	}
	var out Action
	err := a.erasureTransaction(ctx, c.MerchantID, func(ctx context.Context, q *gen.Queries, tx pgx.Tx) error {
		old, err := q.GetDestructiveActionByKey(ctx, gen.GetDestructiveActionByKeyParams{MerchantID: c.MerchantID, ActionKey: c.Key})
		switch {
		case err == nil && old.State != string(StateNoEffect):
			if old.RequestDigest != c.Digest || old.Kind != string(c.Kind) || old.Target != c.Target || old.ActorIssuer != c.Actor.Issuer || old.ActorID != c.Actor.ID {
				return ErrConflict
			}
			out = actionOf(old)
			out.Replayed = true
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := a.clock.Now().UTC()
		row, err := q.CreateDestructiveAction(ctx, gen.CreateDestructiveActionParams{MerchantID: c.MerchantID, ID: id, Kind: string(c.Kind), Target: c.Target, ActionKey: c.Key, RequestDigest: c.Digest, Origin: c.originName(), ActorIssuer: c.Actor.Issuer, ActorID: c.Actor.ID, ActorKey: c.Actor.key(), Credential: optional(c.Actor.Credential), Units: c.Units, AdmittedAt: now})
		if err != nil {
			return err
		}
		out = actionOf(row)
		start := c.originName() == "host"
		if !start {
			if start, err = a.erasureMayStart(ctx, q, tx, row, now); err != nil {
				return err
			}
		}
		if !start {
			return QueueDrain(ctx, a.db, tx, c.MerchantID)
		}
		if row, err = q.StartDestructiveAction(ctx, gen.StartDestructiveActionParams{MerchantID: c.MerchantID, ID: id, Now: now}); err != nil {
			return err
		}
		out = actionOf(row)
		recovery := DrainErasuresArgs{MerchantID: c.MerchantID, Recovers: id.String()}
		later := recovery.InsertOpts()
		later.ScheduledAt = now.Add(StuckErasure + time.Minute)
		_, err = a.db.InsertRiverJobTx(ctx, tx, recovery, &later)
		return err
	})
	return out, err
}

// erasureMayStart reports whether erasure row may start now: nothing waits
// before it, its scope is unheld and its budget has room. Over the budget it
// opens the scope's hold, row its trigger.
func (a *Admissions) erasureMayStart(ctx context.Context, q *gen.Queries, tx pgx.Tx, row gen.BillingDestructiveAction, now time.Time) (bool, error) {
	key := erasureKey(row.MerchantID)
	if held, err := openHold(ctx, q, key); err != nil || held {
		return false, err
	}
	queued, err := q.ErasureQueuedBefore(ctx, gen.ErasureQueuedBeforeParams{MerchantID: row.MerchantID, AdmissionSequence: row.AdmissionSequence})
	if err != nil || queued {
		return false, err
	}
	budget, err := a.erasureBudget(ctx, q, row.MerchantID, now)
	if err != nil || budget.room > 0 {
		return budget.room > 0, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	h, err := q.CreateDestructiveHold(ctx, gen.CreateDestructiveHoldParams{MerchantID: row.MerchantID, ID: id, Scope: string(ScopeErasure), ScopeKey: key, ActorIssuer: row.ActorIssuer, ActorID: row.ActorID, Credential: row.Credential, ActionKind: row.Kind, Target: row.Target, WindowName: budget.window, Observed: budget.count, Requested: row.Units, Ceiling: budget.limit, OpenedAt: now})
	if err != nil {
		return false, err
	}
	if err := a.project(ctx, tx, holdOf(h)); err != nil {
		return false, fmt.Errorf("project erasure hold: %w", err)
	}
	return false, nil
}

func openHold(ctx context.Context, q *gen.Queries, key string) (bool, error) {
	_, err := q.FindOpenDestructiveHold(ctx, []string{key})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// erasureBudget is what the merchant's erasure scope has left: the tighter of
// its hour and day, each the configured floor or the directory's share,
// whichever is larger.
type erasureBudget struct {
	room, count, limit int64
	window             string
}

func (a *Admissions) erasureBudget(ctx context.Context, q *gen.Queries, mid uuid.UUID, now time.Time) (erasureBudget, error) {
	after, err := q.DestructiveScopeClearEpoch(ctx, erasureKey(mid))
	if err != nil {
		return erasureBudget{}, err
	}
	n, err := q.CountErasureUnits(ctx, gen.CountErasureUnitsParams{MerchantID: mid, HourSince: now.Add(-time.Hour), DaySince: now.Add(-24 * time.Hour), ClearAfter: after})
	if err != nil {
		return erasureBudget{}, err
	}
	hourLimit, dayLimit := a.policy.ErasurePerHour, a.policy.ErasurePerDay
	// Within its floors a scope reads no directory. Past one it counts only
	// the provisioned contacts that would lift the exceeded window by one,
	// an index-only read (customer_contacts_provisioned_idx) that stops there.
	var required int64
	if n.Hourly >= hourLimit {
		required = n.Hourly*erasureHourShare + 1
	}
	if n.Daily >= dayLimit {
		required = max(required, n.Daily*erasureDayShare+1)
	}
	if required > 0 {
		contacts, err := q.ProvisionedContactsAtLeast(ctx, gen.ProvisionedContactsAtLeastParams{MerchantID: mid, Required: required})
		if err != nil {
			return erasureBudget{}, err
		}
		hourLimit, dayLimit = max(hourLimit, ceil(contacts, erasureHourShare)), max(dayLimit, ceil(contacts, erasureDayShare))
	}
	if hourLimit-n.Hourly <= dayLimit-n.Daily {
		return erasureBudget{room: max(hourLimit-n.Hourly, 0), count: n.Hourly, limit: hourLimit, window: "hour"}, nil
	}
	return erasureBudget{room: max(dayLimit-n.Daily, 0), count: n.Daily, limit: dayLimit, window: "day"}, nil
}

// StartErasures starts the merchant's waiting erasures, oldest first, as far
// as its budget allows while its scope is unheld, and hands back those a crash
// left started (StuckErasure) to run again. Waiting reports any left queued.
func (a *Admissions) StartErasures(ctx context.Context, mid uuid.UUID, limit int32) (out []Action, waiting bool, err error) {
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("start 1 to 1000 erasures")
	}
	err = a.erasureTransaction(ctx, mid, func(ctx context.Context, q *gen.Queries, tx pgx.Tx) error {
		now := a.clock.Now().UTC()
		rows, err := q.ListErasureQueue(ctx, gen.ListErasureQueueParams{MerchantID: mid, StuckBefore: now.Add(-StuckErasure), RowLimit: limit + 1})
		if err != nil {
			return err
		}
		var room int64
		held, err := openHold(ctx, q, erasureKey(mid))
		if err != nil {
			return err
		}
		if !held {
			budget, err := a.erasureBudget(ctx, q, mid, now)
			if err != nil {
				return err
			}
			room = budget.room
		}
		for i, row := range rows {
			if i == int(limit) {
				waiting = true
				break
			}
			if row.State == string(StateStarted) {
				out = append(out, actionOf(row))
				continue
			}
			if room <= 0 {
				waiting = true
				continue
			}
			started, err := q.StartDestructiveAction(ctx, gen.StartDestructiveActionParams{MerchantID: mid, ID: row.ID, Now: now})
			if err != nil {
				return err
			}
			out = append(out, actionOf(started))
			room--
		}
		return nil
	})
	return out, waiting, err
}

// DrainErasuresKind is the erasure drain's job kind.
const DrainErasuresKind = "openrails.contact_erasures"

// DrainErasuresArgs is the due work that drains a merchant's erasure queue:
// queued when an erasure waits and when its scope's hold is cleared
// (QueueDrain), and for each started erasure when it would be stuck.
type DrainErasuresArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	// Follows is the running drain this one was queued behind: that drain
	// may have read the queue before the erasure now waiting.
	Follows int64 `json:"follows,omitempty" river:"unique"`
	// Recovers is the started erasure this drain runs again should its run
	// not finish; it leaves waiting erasures to the other drains.
	Recovers string `json:"recovers,omitempty" river:"unique"`
}

// Kind names the job.
func (DrainErasuresArgs) Kind() string { return DrainErasuresKind }

// InsertOpts keeps one unfinished drain per args: River's unique states but
// completed, so a finished drain never blocks the next.
func (DrainErasuresArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}

// maxFollows bounds the drains queued behind running ones.
const maxFollows = 4

// QueueDrain queues the merchant's erasure drain to run now, in tx. One
// waiting for later is brought forward; one running may have read the queue
// before tx commits, so another is queued to follow it.
func QueueDrain(ctx context.Context, d *db.DB, tx pgx.Tx, mid uuid.UUID) error {
	args := DrainErasuresArgs{MerchantID: mid}
	for range maxFollows {
		res, err := d.InsertRiverJobTx(ctx, tx, args, nil)
		if err != nil || res == nil || !res.UniqueSkippedAsDuplicate || res.Job == nil {
			return err
		}
		switch res.Job.State {
		case rivertype.JobStateRunning:
			args.Follows = res.Job.ID
		case rivertype.JobStateScheduled, rivertype.JobStateRetryable:
			return d.RetryRiverJobTx(ctx, tx, res.Job.ID)
		default:
			return nil
		}
	}
	return fmt.Errorf("queue the erasure drain: %d drains of merchant %s are running", maxFollows, mid)
}
