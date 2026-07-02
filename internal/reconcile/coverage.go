package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

func timePtrIfSet(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t.UTC()
	return &tt
}

// PullProof attests one provider's COMPLETED pull (no fetch error, no
// circuit-breaker abort, mirror writes applied): the coverage its fetcher
// declared and the account binding it ran under ("" = the rail's configured
// credentials, account-agnostic).
type PullProof struct {
	Coverage              SnapshotCoverage
	RailMerchantAccountID string
}

// PullProofs maps each proven provider to its pull proof.
type PullProofs map[Provider]PullProof

// Merge folds another proof set in (later proofs win per provider).
func (p PullProofs) Merge(o PullProofs) {
	for provider, proof := range o {
		p[provider] = proof
	}
}

// PullProofs extracts the completed provider sections of a run.
func (r *RunResult) PullProofs() PullProofs {
	out := PullProofs{}
	if r == nil || r.Summary == nil {
		return out
	}
	for name, rep := range r.Summary.Providers {
		if rep == nil || rep.Aborted || rep.Error != "" {
			continue
		}
		out[Provider(name)] = PullProof{Coverage: rep.Coverage, RailMerchantAccountID: rep.RailMerchantAccountID}
	}
	return out
}

// MarkReconciledSourceDomains flips the §3.2 confirmed-absence gate
// (openrails.reconciliation_state) for every source domain the given pull
// proofs actually PROVE, and returns the domains flipped. The rule (#665):
//
//   - Absence proofs need EXHAUSTIVE coverage (SnapshotCoverage), never mere
//     event-window watermark freshness.
//   - A domain is proven only when EVERY rail in the merchant's declared
//     rail_merchant_accounts catalog was covered — and only rails with exactly
//     ONE declared account (any environment) qualify, because a single
//     credentials-set pull cannot prove absence across sibling accounts. An
//     account-bound pull must be bound to that one account. An empty catalog
//     proves nothing (accounts may simply not be declared yet).
//   - `subscriptions` needs SubscriptionsExhaustive (rosters are not
//     windowed, so bounded event pulls still prove it). `payments` needs
//     exhaustive, pagination-complete, UNBOUNDED transaction coverage (full
//     history) — only a full-head pull proves it. `grants` is admin/local-
//     sourced: no pull ever proves it (manual/bulk-import decision only).
//   - The flag is a ratchet: proven domains flip true; nothing is unset.
//
// Callers run it merchant-scoped (RLS) after mirror writes were applied
// (enforce insert+overwrite) — an advisory dry-run proves nothing about the
// LOCAL mirror.
func MarkReconciledSourceDomains(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, proofs PullProofs, now time.Time) ([]string, error) {
	accounts, err := q.ListRailMerchantAccountsForMerchant(ctx, gen.ListRailMerchantAccountsForMerchantParams{
		MerchantID: merchantID,
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: list rail merchant accounts: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	byRail := map[string][]gen.OpenrailsRailMerchantAccount{}
	for i := range accounts {
		byRail[accounts[i].Rail] = append(byRail[accounts[i].Rail], accounts[i])
	}

	subsProven, paymentsProven := true, true
	for rail, accts := range byRail {
		proof, ok := proofs[Provider(rail)]
		if !ok || len(accts) != 1 ||
			(proof.RailMerchantAccountID != "" && proof.RailMerchantAccountID != accts[0].ID.String()) {
			subsProven, paymentsProven = false, false
			break
		}
		cov := proof.Coverage
		subsProven = subsProven && cov.SubscriptionsExhaustive
		paymentsProven = paymentsProven && cov.TransactionsExhaustive &&
			cov.TransactionsPaginatedComplete && cov.TransactionWindowSince == nil
	}

	var flipped []string
	for domain, proven := range map[string]bool{"subscriptions": subsProven, "payments": paymentsProven} {
		if !proven {
			continue
		}
		ts := now.UTC()
		if _, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: domain, FullyReconciled: true, LastFullPullAt: &ts,
		}); err != nil {
			return flipped, fmt.Errorf("reconcile: mark %s reconciled: %w", domain, err)
		}
		flipped = append(flipped, domain)
	}
	return flipped, nil
}
