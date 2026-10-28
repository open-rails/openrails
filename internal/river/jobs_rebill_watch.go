package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/internal/writeposture"
)

const (
	KindRebillWatch = "openrails.rebill_watch"

	// RebillWatchInterval is how often overdue rebills are looked for.
	RebillWatchInterval = 15 * time.Minute
	// EngineRebillDeadline: the due pass admits an engine renewal within
	// minutes of its boundary.
	EngineRebillDeadline = time.Hour
	// SolanaRebillDeadline: a Solana renewal is pulled by the crank, not the
	// due pass. It starts at the first crank after its boundary, a failed
	// crank retries at the next, and its confirmation has the engine's
	// deadline.
	SolanaRebillDeadline = 2*SolanaCrankInterval + EngineRebillDeadline
	// NMIRebillDeadline: NMI charges once on the schedule day, and its Query
	// API indexes a charge with some lag.
	NMIRebillDeadline = 24 * time.Hour

	// FindingRebillMissed is one cycle whose rebill never happened.
	FindingRebillMissed = "life.rebill.missed"

	// MissProviderSkipped: NMI's schedule moved past the period without
	// charging it, and OpenRails collects it.
	MissProviderSkipped = "provider_skipped"
	// MissProviderReversed: NMI charged the cycle and the charge was voided
	// or refunded in full. OpenRails never charges it again.
	MissProviderReversed = "provider_reversed"
	// MissProviderUnrecorded: NMI holds a transaction in the cycle that no
	// attempt records. OpenRails never charges the cycle.
	MissProviderUnrecorded = "provider_unrecorded"

	rebillWatchMerchantBatch = 500
)

// RebillWatchArgs runs one pass of the missed-rebill watch.
type RebillWatchArgs struct{}

func (RebillWatchArgs) Kind() string { return KindRebillWatch }

func (RebillWatchArgs) InsertOpts() river.InsertOpts { return periodPass(RebillWatchInterval) }

// RebillWatchWorker records rebills that never happened: an auto-renewing
// subscription whose period ended past its owner's deadline with no attempt
// for that cycle. An NMI-owned one is probed first: the Query API is the
// backstop for a lost webhook, so a charge found there is recorded and
// converged, and only a proven absence is a miss. A period NMI's schedule
// skipped is then collected by OpenRails.
type RebillWatchWorker struct {
	WorkerDefaults[RebillWatchArgs]
	DB          *db.DB
	Config      *config.Config
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
	Lifecycle   *subscriptions.SubscriptionLifecycleService
}

func (RebillWatchWorker) Kind() string { return KindRebillWatch }

func (w *RebillWatchWorker) Work(ctx context.Context, _ *river.Job[RebillWatchArgs]) error {
	now := w.Clock.Now().UTC()
	cutoffs := subscriptions.RebillCutoffs{Engine: now.Add(-EngineRebillDeadline), Solana: now.Add(-SolanaRebillDeadline), NMI: now.Add(-NMIRebillDeadline)}
	var workErr error
	after := uuid.Nil
	for {
		merchantIDs, err := w.DB.GenDirectory().ListOverdueRebillMerchants(ctx, gen.ListOverdueRebillMerchantsParams{
			EngineCutoff: cutoffs.Engine, SolanaCutoff: cutoffs.Solana, NmiCutoff: cutoffs.NMI, Now: now, AfterMerchantID: after, MerchantLimit: rebillWatchMerchantBatch,
		})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("rebill watch: list merchants: %w", err))
		}
		for _, mid := range merchantIDs {
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "rebill watch", func(ctx context.Context) error {
				return w.watchMerchant(ctx, cutoffs, now)
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", mid, err))
			}
		}
		if len(merchantIDs) < rebillWatchMerchantBatch {
			return workErr
		}
		after = merchantIDs[len(merchantIDs)-1]
	}
}

// watchMerchant decides each of the merchant's overdue cycles, a page at a
// time. A cycle whose look fails waits out its backoff, so it never holds up
// the cycles after it.
func (w *RebillWatchWorker) watchMerchant(ctx context.Context, cutoffs subscriptions.RebillCutoffs, now time.Time) error {
	repo := subscriptions.NewSubscriptionRepo(w.DB)
	var after subscriptions.DueDunningCursor
	var merchantErr error
	for {
		subs, err := repo.ListOverdueRebills(ctx, cutoffs, now, after)
		if err != nil {
			return errors.Join(merchantErr, err)
		}
		for _, sub := range subs {
			if err := w.watch(ctx, sub, now); err != nil {
				next, rerr := w.backOff(ctx, sub, now)
				log.WithContext(ctx).WithError(errors.Join(err, rerr)).WithFields(log.Fields{"subscription_id": sub.ID, "next_look_at": next}).
					Warn("rebill watch: subscription left open; looked at again after a backoff")
				if rerr != nil {
					merchantErr = errors.Join(merchantErr, fmt.Errorf("subscription %s: %w", sub.ID, rerr))
				}
			}
		}
		if len(subs) < subscriptions.DueDunningBatch {
			return merchantErr
		}
		last := subs[len(subs)-1]
		after = subscriptions.DueDunningCursor{DueAt: *last.CurrentPeriodEndsAt, ID: last.ID}
	}
}

// backOff records a failed look at sub's overdue cycle and returns when it is
// looked at again.
func (w *RebillWatchWorker) backOff(ctx context.Context, sub *models.Subscription, now time.Time) (time.Time, error) {
	cycle, err := w.cycle(sub)
	if err != nil {
		return time.Time{}, err
	}
	next, err := w.DB.Gen(ctx).RecordRebillProbeFailure(ctx, gen.RecordRebillProbeFailureParams{
		ID: cycle.ID, MerchantID: cycle.MerchantID, SubscriptionID: cycle.SubscriptionID, CustomerID: cycle.CustomerID, PspID: cycle.PspID, Rail: cycle.Rail,
		Owner: cycle.Owner, DueAt: cycle.DueAt, Amount: cycle.Amount, Currency: cycle.Currency, Quantity: cycle.Quantity, FailedAt: now,
	})
	if err != nil || next == nil {
		return time.Time{}, errors.Join(errors.New("record the failed look"), err)
	}
	return *next, nil
}

// cycle is the rebill cycle of sub's current period.
func (w *RebillWatchWorker) cycle(sub *models.Subscription) (gen.UpsertRebillCycleParams, error) {
	if sub.Price == nil {
		return gen.UpsertRebillCycleParams{}, errors.New("subscription price not loaded")
	}
	amount, err := subscriptions.SeatAmount(sub.Price.Amount, sub.Quantity)
	if err != nil {
		return gen.UpsertRebillCycleParams{}, err
	}
	return gen.UpsertRebillCycleParams{
		ID: uuidutil.NewV7(), MerchantID: sub.MerchantID, SubscriptionID: sub.ID, CustomerID: sub.CustomerID, PspID: sub.PspID, Rail: string(sub.Rail),
		Owner: string(attempts.OwnerOf(sub.CollectionPolicy)), DueAt: sub.CurrentPeriodEndsAt.UTC(), Amount: amount, Currency: sub.Price.Currency,
		Quantity: models.IntPtrTo32(sub.Quantity),
	}, nil
}

// watch decides one overdue cycle.
func (w *RebillWatchWorker) watch(ctx context.Context, sub *models.Subscription, now time.Time) error {
	due := sub.CurrentPeriodEndsAt.UTC()
	if sub.CollectionPolicy == models.CollectionPolicyNMISchedule {
		reason, held, missed, err := w.probeNMI(ctx, sub, now)
		if err != nil || !missed {
			return err
		}
		return w.miss(ctx, sub, due, reason, held, now)
	}
	if open, err := w.DB.Gen(ctx).CountOpenSubscriptionCollections(ctx, gen.CountOpenSubscriptionCollectionsParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID}); err != nil || open > 0 {
		return err // a renewal in flight decides the cycle itself
	}
	return w.miss(ctx, sub, due, w.engineReason(ctx, sub), nil, now)
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
// found is recorded (observed by pull) and decides the cycle. Any other
// transaction NMI holds for the cycle (a voided or refunded sale, a charge off
// the schedule's date or order) is a miss OpenRails never collects. Only when
// NMI holds nothing is the miss named by what its schedule shows.
func (w *RebillWatchWorker) probeNMI(ctx context.Context, sub *models.Subscription, now time.Time) (string, []nmi.CycleTransaction, bool, error) {
	if w.NMIResolver == nil || w.Lifecycle == nil {
		return "", nil, false, errors.New("nmi resolver or lifecycle not wired")
	}
	client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, sub.MerchantID, &sub.PspID)
	if err != nil || !ok || client == nil {
		return "", nil, false, fmt.Errorf("nmi client for %s unavailable: %v", sub.PspID, err)
	}
	due, since := sub.CurrentPeriodEndsAt.UTC(), cycleWindowStart(sub)
	snap, err := (&reconcile.NMISubscriptionProber{Client: client}).ProbeSubscription(ctx, reconcile.ProbeSubject{
		LocalID: sub.ID, RailSubscriptionID: sub.RailSubscriptionID, PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt, ObservedAt: now,
	})
	if err != nil {
		return "", nil, false, err
	}
	if _, err := reconcile.ConvergeSubscriptionFromSnapshot(attempts.ObservedVia(ctx, "pull"), w.DB, w.Lifecycle, sub, snap, now, 0); err != nil {
		return "", nil, false, err
	}
	attempted, err := w.DB.Gen(ctx).CycleHasAttempt(ctx, gen.CycleHasAttemptParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, DueAt: due})
	if err != nil || attempted {
		return "", nil, false, err
	}
	if len(snap.Transactions) > 0 {
		return "", nil, false, errors.New("NMI shows a charge in the cycle that no attempt records")
	}
	scope := nmi.CycleScope{ScheduleID: sub.RailSubscriptionID, OrderIDs: []string{sub.ID.String()}}
	charged, err := subscriptions.PaymentMethodOf(ctx, w.DB.Gen(ctx), sub)
	if err != nil {
		return "", nil, false, err
	}
	if charged != nil {
		method, err := w.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *charged})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, fmt.Errorf("payment method: %w", err)
		}
		scope.VaultID = models.DerefStr(method.RailCustomerRef)
	}
	held, err := client.ReadCycleTransactions(ctx, scope, since)
	if err != nil {
		return "", nil, false, err
	}
	for _, txn := range held {
		if txn.Reversed {
			return MissProviderReversed, held, true, nil
		}
	}
	if len(held) > 0 {
		return MissProviderUnrecorded, held, true, nil
	}
	if len(snap.Subscriptions) == 0 {
		return "schedule_gone", nil, true, nil
	}
	next := snap.Subscriptions[0].NextBillingAt
	if next != nil && next.After(due.Add(reconcile.AlignmentSlack(sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt))) {
		return MissProviderSkipped, nil, true, nil
	}
	return "provider_stalled", nil, true, nil
}

// cycleWindowStart is the earliest a cycle's own transaction can be: half way
// through its period, so neither the charge that opened it nor one off the
// schedule's date by days passes for the other. Unknown bounds read all.
func cycleWindowStart(sub *models.Subscription) time.Time {
	start, end := sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt
	if start == nil || end == nil || !end.After(*start) {
		return time.Time{}
	}
	return start.Add(end.Sub(*start) / 2).UTC()
}

// miss records the cycle as missed and raises its finding with what NMI holds
// for it. A period NMI's schedule skipped (NMI holds nothing and its next
// date moved on) is handed to OpenRails' collection in the same transaction.
func (w *RebillWatchWorker) miss(ctx context.Context, sub *models.Subscription, due time.Time, reason string, held []nmi.CycleTransaction, now time.Time) error {
	params, err := w.cycle(sub)
	if err != nil {
		return err
	}
	collect := reason == MissProviderSkipped && !(writeposture.View{Config: w.Config, DB: w.DB}).Posture(ctx, sub.MerchantID).ReadOnly()
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := w.DB.NewWithPgxTx(tx)
		q := txdb.Gen(ctx)
		cycle, err := q.UpsertRebillCycle(ctx, params)
		if err != nil {
			return err
		}
		if n, err := q.MarkRebillCycleMissed(ctx, gen.MarkRebillCycleMissedParams{MerchantID: sub.MerchantID, ID: cycle, MissedAt: now, MissReason: reason}); err != nil || n == 0 {
			return err
		}
		evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "cycle_id": cycle, "period_end": due, "owner": sub.CollectionPolicy, "reason": reason, "collected": collect, "nmi_transactions": held})
		action := fmt.Sprintf("subscription %s was due to rebill at %s and no attempt happened (%s). Check the provider's schedule and the due pass; the member keeps access until the rebill is resolved.", sub.ID, due.Format(time.RFC3339), reason)
		switch {
		case collect:
			action = fmt.Sprintf("NMI's schedule passed subscription %s's rebill due at %s without charging it. OpenRails charges the period now and duns a decline on the merchant's schedule; check the schedule at NMI.", sub.ID, due.Format(time.RFC3339))
		case len(held) > 0:
			action = fmt.Sprintf("NMI holds %d transaction(s) for subscription %s's rebill due at %s that no attempt records (%s; ids in the evidence). OpenRails will not charge this period. Review them at NMI and settle the period there; the member keeps access until then.", len(held), sub.ID, due.Format(time.RFC3339), reason)
		}
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: sub.MerchantID, FindingType: FindingRebillMissed, SubjectKey: cycle.String(),
			Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
		}); err != nil || !collect {
			return err
		}
		_, err = w.Lifecycle.CollectSkippedRenewal(ctx, txdb, sub)
		return err
	})
}
