package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// An NMI schedule charges its renewal once, on its day. When the verifier
// reads a schedule whose cycle is past NMIRebillDeadline and the read
// recorded no attempt for it, NMI's own records of the cycle decide its miss.

const (
	// NMIRebillDeadline: NMI charges once on the schedule day, and its Query
	// API indexes a charge with some lag.
	NMIRebillDeadline = 24 * time.Hour

	// FindingRebillMissed is one NMI cycle whose rebill never happened.
	FindingRebillMissed = "life.rebill.missed"

	// missProviderSkipped: NMI's schedule moved past the period without
	// charging it, and OpenRails collects it.
	missProviderSkipped = "provider_skipped"
	// missProviderStalled: NMI holds nothing for the cycle and its schedule
	// has not moved past it.
	missProviderStalled = "provider_stalled"
	// missProviderReversed: NMI charged the cycle and the charge was voided
	// or refunded in full. OpenRails never charges it again.
	missProviderReversed = "provider_reversed"
	// missProviderUnrecorded: NMI holds a transaction in the cycle that no
	// attempt records. OpenRails never charges the cycle.
	missProviderUnrecorded = "provider_unrecorded"
	// missScheduleGone: NMI no longer holds the schedule.
	missScheduleGone = "schedule_gone"
)

// overdueNMICycle reports whether sub is an NMI schedule whose current cycle
// is past NMIRebillDeadline at now.
func overdueNMICycle(sub *models.Subscription, now time.Time) bool {
	return sub.CollectionPolicy == models.CollectionPolicyNMISchedule && sub.CurrentPeriodEndsAt != nil &&
		!sub.CurrentPeriodEndsAt.Add(NMIRebillDeadline).After(now)
}

// settleNMICycle decides the cycle due at sub's period end after the read's
// converge: an attempt it recorded (a charge or decline found) or a miss
// already recorded settles it. Otherwise any transaction NMI holds for the
// cycle (voided, refunded, off the schedule's date or order) is a miss
// OpenRails never collects, and with none the read's roster entry names it.
// sub is the row as the read found it.
func settleNMICycle(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, client *nmi.NMIClient, sub *models.Subscription, snap *RemoteSnapshot, now time.Time) error {
	settled, err := database.Gen(ctx).RebillCycleSettled(ctx, gen.RebillCycleSettledParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, DueAt: sub.CurrentPeriodEndsAt.UTC()})
	if err != nil || settled {
		return err
	}
	vaults, err := vaultsOf(ctx, database, []*models.Subscription{sub})
	if err != nil {
		return err
	}
	scope := nmi.CycleScope{ScheduleID: sub.RailSubscriptionID, OrderIDs: []string{sub.ID.String()}, VaultID: vaults[sub.ID]}
	held, err := client.ReadCycleTransactions(ctx, scope, cycleWindowStart(sub))
	if err != nil {
		return fmt.Errorf("nmi cycle transactions: %w", err)
	}
	return missCycle(ctx, database, lc, sub, nmiMissReason(sub, snap, held), held, now)
}

// nmiMissReason names why no attempt records the cycle, from what NMI holds
// for it and the read's roster entry for its schedule.
func nmiMissReason(sub *models.Subscription, snap *RemoteSnapshot, held []nmi.CycleTransaction) string {
	for _, txn := range held {
		if txn.Reversed {
			return missProviderReversed
		}
	}
	if len(held) > 0 {
		return missProviderUnrecorded
	}
	var schedule *RemoteSubscription
	for i := range snap.Subscriptions {
		if snap.Subscriptions[i].RailSubscriptionID == sub.RailSubscriptionID {
			schedule = &snap.Subscriptions[i]
			break
		}
	}
	if schedule == nil {
		return missScheduleGone
	}
	due := sub.CurrentPeriodEndsAt.UTC()
	if next := schedule.NextBillingAt; next != nil && next.After(due.Add(AlignmentSlack(sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt))) {
		return missProviderSkipped
	}
	return missProviderStalled
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

// missCycle records the cycle as missed and raises its finding with what NMI
// holds for it. A period NMI's schedule skipped is handed to OpenRails'
// collection in the same transaction, unless the merchant is readonly.
func missCycle(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, reason string, held []nmi.CycleTransaction, now time.Time) error {
	if sub.Price == nil {
		return errors.New("subscription price not loaded")
	}
	amount, err := subscriptions.SeatAmount(sub.Price.Amount, sub.Quantity)
	if err != nil {
		return err
	}
	due := sub.CurrentPeriodEndsAt.UTC()
	params := gen.UpsertRebillCycleParams{
		ID: uuidutil.NewV7(), MerchantID: sub.MerchantID, SubscriptionID: sub.ID, CustomerID: sub.CustomerID, PspID: sub.PspID, Rail: string(sub.Rail),
		Owner: string(attempts.OwnerOf(sub.CollectionPolicy)), DueAt: due, Amount: amount, Currency: sub.Price.Currency, Quantity: models.IntPtrTo32(sub.Quantity),
	}
	collect := reason == missProviderSkipped && !(controls.View{DB: database}).Posture(ctx, sub.MerchantID).ReadOnly()
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		q := txdb.Gen(ctx)
		cycle, err := q.UpsertRebillCycle(ctx, params)
		if err != nil {
			return err
		}
		if n, err := q.MarkRebillCycleMissed(ctx, gen.MarkRebillCycleMissedParams{MerchantID: sub.MerchantID, ID: cycle, MissedAt: now, MissReason: reason}); err != nil || n == 0 {
			return err
		}
		evidence, _ := json.Marshal(map[string]any{"subscription_id": sub.ID, "cycle_id": cycle, "period_end": due, "owner": sub.CollectionPolicy, "reason": reason, "collected": collect, "nmi_transactions": held})
		action := fmt.Sprintf("subscription %s was due to rebill at %s and no attempt happened (%s). Check the provider's schedule; the member keeps access until the rebill is resolved.", sub.ID, due.Format(time.RFC3339), reason)
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
		_, err = lc.CollectSkippedRenewal(ctx, txdb, sub)
		return err
	})
}
