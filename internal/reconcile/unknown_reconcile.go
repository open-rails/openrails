package reconcile

import "time"

// #632/#633: resolving the `unknown` subscription cohort against provider truth.
// The reconciler pulls each rail's state in ONE windowed bulk fetch (RailFetcher,
// never per-subscription) and matches the cohort locally; this file is the pure
// per-subscription decision, fixture-testable with a hand-built RemoteSnapshot.

// UnknownOutcome is what a provider snapshot says about an `unknown` subscription.
type UnknownOutcome int

const (
	// UnknownOutcomeUnreachable: the snapshot has no conclusive evidence (provider
	// not reached, or the sub wasn't in a non-exhaustive pull) → stay unknown,
	// retried with backoff (#633).
	UnknownOutcomeUnreachable UnknownOutcome = iota
	// UnknownOutcomeRenewed: the provider billed the new period (a successful
	// renewal charge) or the roster confirms the sub is active → return to active.
	UnknownOutcomeRenewed
	// UnknownOutcomePastDue: the renewal FAILED but the lapse is still within the
	// dunning window → past_due (recoverable).
	UnknownOutcomePastDue
	// UnknownOutcomeCancelled: the provider cancelled/deleted the sub, or a failed
	// renewal is older than the dunning window → terminal cancel.
	UnknownOutcomeCancelled
)

// UnknownVerdict is the resolution of one `unknown` subscription.
type UnknownVerdict struct {
	Outcome UnknownOutcome
	// NewPeriodEnd is the provider-confirmed next period end (UnknownOutcomeRenewed).
	NewPeriodEnd *time.Time
	// Backfill are the provider's charge-level events for this subscription at/after
	// the lapsed period end — the missing payments to import (#634), INCLUDING
	// declines/voids (recorded as failed). The orchestration dedups by transaction id.
	Backfill []RemoteTransaction
	// RemoteCustomerID is the provider's card-independent customer object id when it
	// has one (Stripe cus_*, NMI customer_vault_id) — empty for CCBill/Solana. The
	// orchestration materializes a rail_customers row from it for Stripe/NMI (#635).
	RemoteCustomerID string
}

// UnknownSubject is the local `unknown` subscription's matchable fields.
type UnknownSubject struct {
	RailSubscriptionID string
	PeriodEnd          time.Time // current_period_ends_at — the lapsed period boundary
}

// ResolveUnknownFromSnapshot decides what a provider snapshot says about one
// `unknown` subscription (#632 resolution / #633 matching). PURE — no DB, no IO —
// so the whole decision table is fixture-testable. dunningWindow bounds how far
// past the period end a FAILED renewal is still recoverable (past_due) vs terminal.
func ResolveUnknownFromSnapshot(sub UnknownSubject, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) UnknownVerdict {
	v := resolveUnknownOutcome(sub, snap, now, dunningWindow)
	// Carry the provider's card-independent customer id (Stripe cus_*, NMI vault)
	// so the orchestration can materialize a rail_customers row (#635).
	if snap != nil {
		for i := range snap.Subscriptions {
			if snap.Subscriptions[i].RailSubscriptionID == sub.RailSubscriptionID {
				v.RemoteCustomerID = snap.Subscriptions[i].CustomerID
				break
			}
		}
	}
	return v
}

func resolveUnknownOutcome(sub UnknownSubject, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) UnknownVerdict {
	if snap == nil {
		return UnknownVerdict{Outcome: UnknownOutcomeUnreachable}
	}
	// #664: no provider handle = unmatchable; "absent from an exhaustive roster"
	// would be vacuously true and guess-cancel it. Stay unknown.
	if sub.RailSubscriptionID == "" {
		return UnknownVerdict{Outcome: UnknownOutcomeUnreachable}
	}

	// This subscription's charge events + roster entry from the windowed pull.
	var txns []RemoteTransaction
	for i := range snap.Transactions {
		if snap.Transactions[i].SubscriptionID == sub.RailSubscriptionID {
			txns = append(txns, snap.Transactions[i])
		}
	}
	var remoteSub *RemoteSubscription
	for i := range snap.Subscriptions {
		if snap.Subscriptions[i].RailSubscriptionID == sub.RailSubscriptionID {
			remoteSub = &snap.Subscriptions[i]
			break
		}
	}
	backfill := subscriptionBackfill(txns, sub.PeriodEnd)

	// Latest successful renewal vs latest decline AT/AFTER the lapsed period end.
	var renewTxn, declineTxn *RemoteTransaction
	for i := range txns {
		t := &txns[i]
		if t.OccurredAt.Before(sub.PeriodEnd) {
			continue
		}
		switch {
		case t.Type == TransactionTypeSale && t.Success:
			if renewTxn == nil || t.OccurredAt.After(renewTxn.OccurredAt) {
				renewTxn = t
			}
		case t.Type == TransactionTypeDecline || (t.Type == TransactionTypeSale && !t.Success):
			if declineTxn == nil || t.OccurredAt.After(declineTxn.OccurredAt) {
				declineTxn = t
			}
		}
	}

	// 1) A successful renewal charge → the provider billed the new period.
	if renewTxn != nil {
		return UnknownVerdict{Outcome: UnknownOutcomeRenewed, NewPeriodEnd: remoteNextEnd(remoteSub), Backfill: backfill}
	}
	// 2) The roster says the sub is active (provider confirms current, even if the
	//    renewal charge fell outside the pulled transaction window).
	if remoteSub != nil && remoteSub.Status == SubscriptionStatusActive {
		return UnknownVerdict{Outcome: UnknownOutcomeRenewed, NewPeriodEnd: remoteNextEnd(remoteSub), Backfill: backfill}
	}
	// 3) A failed/declined renewal: past_due if still recoverable, else terminal.
	if declineTxn != nil {
		if now.Sub(sub.PeriodEnd) <= dunningWindow {
			return UnknownVerdict{Outcome: UnknownOutcomePastDue, Backfill: backfill}
		}
		return UnknownVerdict{Outcome: UnknownOutcomeCancelled, Backfill: backfill}
	}
	// 4) The provider says cancelled/expired.
	if remoteSub != nil && (remoteSub.Status == SubscriptionStatusCancelled || remoteSub.Status == SubscriptionStatusExpired) {
		return UnknownVerdict{Outcome: UnknownOutcomeCancelled, Backfill: backfill}
	}
	// 5) The sub is absent AND the pull exhaustively covered subscriptions → it was
	//    deleted at the provider.
	if remoteSub == nil && snap.Coverage.SubscriptionsExhaustive {
		return UnknownVerdict{Outcome: UnknownOutcomeCancelled}
	}
	// 6) No conclusive evidence → stay unknown, retry later with backoff.
	return UnknownVerdict{Outcome: UnknownOutcomeUnreachable, Backfill: backfill}
}

// remoteNextEnd is the provider's declared next billing time, or nil.
func remoteNextEnd(s *RemoteSubscription) *time.Time {
	if s == nil {
		return nil
	}
	return s.NextBillingAt
}

// subscriptionBackfill returns the charge events for one subscription at/after the
// lapsed period end (#634): the candidate missing payments — successes AND
// declines/voids — for the orchestration to import idempotently by transaction id.
func subscriptionBackfill(txns []RemoteTransaction, since time.Time) []RemoteTransaction {
	var out []RemoteTransaction
	for i := range txns {
		if !txns[i].OccurredAt.Before(since) {
			out = append(out, txns[i])
		}
	}
	return out
}
