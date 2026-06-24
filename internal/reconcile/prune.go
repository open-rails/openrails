package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PruneParams bounds a #511 pull-provider --prune pass.
type PruneParams struct {
	Since time.Time
	Until time.Time
	Apply bool // false = dry-run (discover + count, write nothing)
}

// PruneResult tallies one provider account's prune pass.
type PruneResult struct {
	Subscriptions          int // deleted (Apply) or would-delete (dry-run)
	SubscriptionsSkipped   int // excess but entangled with the #514 grant ledger
	Payments               int
	PaymentsSkipped        int // excess but with protected dependents
	SubscriptionIDs        []uuid.UUID
	SkippedSubscriptionIDs []uuid.UUID
	PaymentIDs             []uuid.UUID
	SkippedPaymentIDs      []uuid.UUID
	SubscriptionSkipReason string
	PaymentSkipReason      string
}

// PruneProviderAccountExcess fetches the provider's current snapshot for the
// bound account and prunes local mirror rows attributed to that provider account
// that are ABSENT from the snapshot. It is account-bound (legacy NULL-binding
// import rows are never touched — they fall outside the provider_account_id
// filter) and safe by construction:
//
//   - A subscription that fed the #514 grant ledger is SKIPPED — deleting its row
//     would orphan an append-only grant; such excess is retracted through
//     convergence (grant revoke), not row deletion.
//   - A payment with protected dependents (a grant, a refund, an entitlement
//     grant, or a checkout session) is SKIPPED for the same reason.
//   - Everything actually deleted happens inside a per-record merchant-scoped
//     transaction, so a partial failure rolls back rather than corrupting.
//
// Dry-run (Apply=false) only discovers and counts; it writes nothing. It must be
// called inside a merchant-scoped connection (the CLI's RunInMerchantConn).
func PruneProviderAccountExcess(ctx context.Context, database *db.DB, fetcher ProcessorFetcher, provider Provider, binding ProviderAccountBinding, params PruneParams) (PruneResult, error) {
	var res PruneResult
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return res, err
	}
	mid := merchantID.UUID()

	snap, err := fetcher.Fetch(ctx, FetchParams{Since: params.Since, Until: params.Until, ProviderAccountID: binding.ID.String()})
	if err != nil {
		return res, fmt.Errorf("fetch snapshot: %w", err)
	}
	if snap == nil {
		return res, fmt.Errorf("provider %s returned a nil snapshot", provider)
	}

	presentSubs := make([]string, 0, len(snap.Subscriptions))
	for i := range snap.Subscriptions {
		if id := snap.Subscriptions[i].ProcessorSubscriptionID; id != "" {
			presentSubs = append(presentSubs, id)
		}
	}
	presentTxns := make([]string, 0, len(snap.Transactions))
	for i := range snap.Transactions {
		if id := snap.Transactions[i].TransactionID; id != "" {
			presentTxns = append(presentTxns, id)
		}
	}

	q := database.Gen(ctx)

	// --- excess subscriptions (head state: a snapshot lists every active sub) ---
	if !snap.Coverage.CanPruneSubscriptions() {
		skipped, err := q.ListExcessSubscriptionsForProviderAccount(ctx, gen.ListExcessSubscriptionsForProviderAccountParams{
			MerchantID: mid, ProviderAccountID: binding.ID, PresentIds: nil,
		})
		if err != nil {
			return res, fmt.Errorf("list subscription prune skips: %w", err)
		}
		res.SubscriptionsSkipped = len(skipped)
		res.SkippedSubscriptionIDs = append(res.SkippedSubscriptionIDs, skipped...)
		res.SubscriptionSkipReason = "snapshot_not_complete"
	} else {
		excessSubs, err := q.ListExcessSubscriptionsForProviderAccount(ctx, gen.ListExcessSubscriptionsForProviderAccountParams{
			MerchantID: mid, ProviderAccountID: binding.ID, PresentIds: presentSubs,
		})
		if err != nil {
			return res, fmt.Errorf("list excess subscriptions: %w", err)
		}
		for _, subID := range excessSubs {
			entangled, err := q.SubscriptionHasGrant(ctx, gen.SubscriptionHasGrantParams{MerchantID: mid, SubscriptionID: subID})
			if err != nil {
				return res, fmt.Errorf("subscription grant check %s: %w", subID, err)
			}
			if entangled {
				res.SubscriptionsSkipped++
				res.SkippedSubscriptionIDs = append(res.SkippedSubscriptionIDs, subID)
				continue
			}
			if !params.Apply {
				res.Subscriptions++
				res.SubscriptionIDs = append(res.SubscriptionIDs, subID)
				continue
			}
			// Delete dependent mirror rows then the subscription atomically. Payments
			// (subscription_id) FK is ON DELETE SET NULL; solana_subscriptions cascades.
			if err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				tq := gen.New(tx)
				if _, e := tq.PruneDeleteCheckoutSessionsBySubscription(ctx, gen.PruneDeleteCheckoutSessionsBySubscriptionParams{MerchantID: mid, SubscriptionID: subID}); e != nil {
					return e
				}
				if _, e := tq.PruneDeleteEntitlementsBySubscription(ctx, gen.PruneDeleteEntitlementsBySubscriptionParams{MerchantID: mid, SubscriptionID: subID}); e != nil {
					return e
				}
				if _, e := tq.PruneDeleteSubscriptionByID(ctx, gen.PruneDeleteSubscriptionByIDParams{MerchantID: mid, ID: subID}); e != nil {
					return e
				}
				return nil
			}); err != nil {
				return res, fmt.Errorf("delete excess subscription %s: %w", subID, err)
			}
			res.Subscriptions++
			res.SubscriptionIDs = append(res.SubscriptionIDs, subID)
		}
	}

	// --- excess payments (windowed: a snapshot only proves absence in its window) ---
	var since, until *time.Time
	if !params.Since.IsZero() {
		s := params.Since
		since = &s
	}
	if !params.Until.IsZero() {
		u := params.Until
		until = &u
	}
	if !canPruneTransactionWindow(snap.Coverage, params.Since, params.Until) {
		skipped, err := q.ListExcessPaymentsForProviderAccount(ctx, gen.ListExcessPaymentsForProviderAccountParams{
			MerchantID: mid, ProviderAccountID: binding.ID, PresentTxns: nil, Since: since, Until: until,
		})
		if err != nil {
			return res, fmt.Errorf("list payment prune skips: %w", err)
		}
		res.PaymentsSkipped = len(skipped)
		res.SkippedPaymentIDs = append(res.SkippedPaymentIDs, skipped...)
		res.PaymentSkipReason = "snapshot_not_complete"
	} else {
		excessPays, err := q.ListExcessPaymentsForProviderAccount(ctx, gen.ListExcessPaymentsForProviderAccountParams{
			MerchantID: mid, ProviderAccountID: binding.ID, PresentTxns: presentTxns, Since: since, Until: until,
		})
		if err != nil {
			return res, fmt.Errorf("list excess payments: %w", err)
		}
		for _, payID := range excessPays {
			protected, err := q.PaymentHasProtectedDependents(ctx, gen.PaymentHasProtectedDependentsParams{MerchantID: mid, PaymentID: payID})
			if err != nil {
				return res, fmt.Errorf("payment dependent check %s: %w", payID, err)
			}
			if protected != nil && *protected {
				res.PaymentsSkipped++
				res.SkippedPaymentIDs = append(res.SkippedPaymentIDs, payID)
				continue
			}
			if !params.Apply {
				res.Payments++
				res.PaymentIDs = append(res.PaymentIDs, payID)
				continue
			}
			if _, e := q.PruneDeletePaymentByID(ctx, gen.PruneDeletePaymentByIDParams{MerchantID: mid, ID: payID}); e != nil {
				return res, fmt.Errorf("delete excess payment %s: %w", payID, e)
			}
			res.Payments++
			res.PaymentIDs = append(res.PaymentIDs, payID)
		}
	}
	return res, nil
}

func canPruneTransactionWindow(coverage SnapshotCoverage, since, until time.Time) bool {
	if !coverage.CanPruneTransactions() {
		return false
	}
	return sameCoverageTime(coverage.TransactionWindowSince, since) &&
		sameCoverageTime(coverage.TransactionWindowUntil, until)
}

func sameCoverageTime(got *time.Time, want time.Time) bool {
	if got == nil || want.IsZero() {
		return false
	}
	return got.UTC().Equal(want.UTC())
}
