package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindRebillWatch = "openrails.rebill_watch"

	// RebillWatchInterval is how often overdue rebills are looked for.
	RebillWatchInterval = 15 * time.Minute
	// EngineRebillDeadline: the due pass admits an engine renewal within
	// minutes of its boundary.
	EngineRebillDeadline = time.Hour
	// NMIRebillDeadline: NMI charges once on the schedule day, and its Query
	// API indexes a charge with some lag.
	NMIRebillDeadline = 24 * time.Hour

	// FindingRebillMissed is one cycle whose rebill never happened.
	FindingRebillMissed = "life.rebill.missed"

	rebillWatchMerchantBatch = 500
)

// RebillWatchArgs runs one pass of the missed-rebill watch.
type RebillWatchArgs struct{}

func (RebillWatchArgs) Kind() string { return KindRebillWatch }

// RebillWatchWorker records rebills that never happened (#1112): an
// auto-renewing subscription whose period ended past its owner's deadline
// with no attempt for that cycle. An NMI-owned one is probed first: the Query
// API is the backstop for a lost webhook, so a charge found there is recorded
// and converged, and only a proven absence is a miss.
type RebillWatchWorker struct {
	river.WorkerDefaults[RebillWatchArgs]
	DB          *db.DB
	Config      *config.Config
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
	Lifecycle   *subscriptions.SubscriptionLifecycleService
}

func (RebillWatchWorker) Kind() string { return KindRebillWatch }

func (w *RebillWatchWorker) Work(ctx context.Context, _ *river.Job[RebillWatchArgs]) error {
	now := w.Clock.Now().UTC()
	engineCutoff, nmiCutoff := now.Add(-EngineRebillDeadline), now.Add(-NMIRebillDeadline)
	merchantIDs, err := w.DB.GenDirectory().ListOverdueRebillMerchants(ctx, gen.ListOverdueRebillMerchantsParams{
		EngineCutoff: engineCutoff, NmiCutoff: nmiCutoff, MerchantLimit: rebillWatchMerchantBatch,
	})
	if err != nil {
		return fmt.Errorf("rebill watch: list merchants: %w", err)
	}
	var workErr error
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		err := w.DB.RunInMerchantScope(ctx, merchant.ID(*mid), "rebill watch", func(ctx context.Context) error {
			subs, err := subscriptions.NewSubscriptionRepo(w.DB).ListOverdueRebills(ctx, engineCutoff, nmiCutoff)
			if err != nil {
				return err
			}
			for _, sub := range subs {
				if err := w.watch(ctx, sub, now); err != nil {
					log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("rebill watch: subscription left open; retried next pass")
				}
			}
			return nil
		})
		if err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", *mid, err))
		}
	}
	return workErr
}

// watch decides one overdue cycle.
func (w *RebillWatchWorker) watch(ctx context.Context, sub *models.Subscription, now time.Time) error {
	due := sub.CurrentPeriodEndsAt.UTC()
	if sub.CollectionPolicy == models.CollectionPolicyNMISchedule {
		reason, missed, err := w.probeNMI(ctx, sub, now)
		if err != nil || !missed {
			return err
		}
		return w.miss(ctx, sub, due, reason, now)
	}
	if open, err := w.DB.Gen(ctx).CountOpenSubscriptionCollections(ctx, gen.CountOpenSubscriptionCollectionsParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID}); err != nil || open > 0 {
		return err // a renewal in flight decides the cycle itself
	}
	return w.miss(ctx, sub, due, w.engineReason(ctx, sub), now)
}

func (w *RebillWatchWorker) engineReason(ctx context.Context, sub *models.Subscription) string {
	switch {
	case w.Config != nil && w.Config.EngineAdmissionHold:
		return "held"
	case sub.Status == models.StatusAwaitingMethod:
		return "method_unusable"
	}
	finding, err := w.DB.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: sub.MerchantID, FindingType: FindingDuePassRefused, SubjectKey: sub.ID.String()})
	if err == nil && finding.ResolvedAt == nil {
		return "refused"
	}
	return "not_attempted"
}

// probeNMI fetches NMI's record of the cycle and converges on it. A charge
// found is recorded (observed by pull) and decides the cycle; none found is a
// miss, named by what NMI's schedule shows.
func (w *RebillWatchWorker) probeNMI(ctx context.Context, sub *models.Subscription, now time.Time) (string, bool, error) {
	if w.NMIResolver == nil || w.Lifecycle == nil {
		return "", false, errors.New("nmi resolver or lifecycle not wired")
	}
	client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, sub.MerchantID, &sub.PspID)
	if err != nil || !ok || client == nil {
		return "", false, fmt.Errorf("nmi client for %s unavailable: %v", sub.PspID, err)
	}
	snap, err := (&reconcile.NMISubscriptionProber{Client: client}).ProbeSubscription(ctx, reconcile.ProbeSubject{
		LocalID: sub.ID, RailSubscriptionID: sub.RailSubscriptionID, PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt, ObservedAt: now,
	})
	if err != nil {
		return "", false, err
	}
	due := sub.CurrentPeriodEndsAt.UTC()
	if _, err := reconcile.ConvergeSubscriptionFromSnapshot(attempts.ObservedVia(ctx, "pull"), w.DB, w.Lifecycle, sub, snap, now, 0); err != nil {
		return "", false, err
	}
	attempted, err := w.DB.Gen(ctx).CycleHasAttempt(ctx, gen.CycleHasAttemptParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, DueAt: due})
	if err != nil || attempted {
		return "", false, err
	}
	if len(snap.Subscriptions) == 0 {
		return "schedule_gone", true, nil
	}
	next := snap.Subscriptions[0].NextBillingAt
	if next != nil && next.After(due.Add(reconcile.AlignmentSlack(sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt))) {
		return "provider_skipped", true, nil
	}
	return "provider_stalled", true, nil
}

// miss records the cycle as missed and raises its finding.
func (w *RebillWatchWorker) miss(ctx context.Context, sub *models.Subscription, due time.Time, reason string, now time.Time) error {
	if sub.Price == nil {
		return errors.New("subscription price not loaded")
	}
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := w.DB.NewWithPgxTx(tx).Gen(ctx)
		cycle, err := q.UpsertRebillCycle(ctx, gen.UpsertRebillCycleParams{
			ID: uuidutil.NewV7(), MerchantID: sub.MerchantID, SubscriptionID: sub.ID, CustomerID: sub.CustomerID, PspID: sub.PspID, Rail: string(sub.Rail),
			Owner: string(attempts.OwnerOf(sub.CollectionPolicy)), DueAt: due, Amount: sub.Price.Amount, Currency: sub.Price.Currency,
		})
		if err != nil {
			return err
		}
		if n, err := q.MarkRebillCycleMissed(ctx, gen.MarkRebillCycleMissedParams{MerchantID: sub.MerchantID, ID: cycle, MissedAt: now, MissReason: reason}); err != nil || n == 0 {
			return err
		}
		evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "cycle_id": cycle, "due_at": due, "owner": sub.CollectionPolicy, "reason": reason})
		action := fmt.Sprintf("subscription %s was due to rebill at %s and no attempt happened (%s). Check the provider's schedule and the due pass; the member keeps access until the rebill is resolved.", sub.ID, due.Format(time.RFC3339), reason)
		_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: sub.MerchantID, FindingType: FindingRebillMissed, SubjectKey: cycle.String(),
			Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
		})
		return err
	})
}
