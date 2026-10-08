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
	Coverage SnapshotCoverage
	PspID    string
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
	if r == nil || r.Summary == nil || r.Mode != ModeEnforce {
		return out
	}
	for name, rep := range r.Summary.Providers {
		if rep == nil || rep.Aborted || rep.Error != "" || len(rep.ApplyErrors) != 0 ||
			rep.WithheldChanges != 0 || rep.RequiresReview != 0 || r.hasUnapplied(name, false) {
			continue
		}
		out[Provider(name)] = PullProof{Coverage: rep.Coverage, PspID: rep.PspID}
	}
	return out
}

// AppliedEventCoverage confirms the financial part of an observed provider
// window. Policy-held lifecycle transitions do not prevent receipt recovery;
// this does not authorize those transitions or prove provider finality.
func (r *RunResult) AppliedEventCoverage(provider Provider, since, until time.Time) bool {
	if r == nil || r.Mode != ModeEnforce || r.Summary == nil {
		return false
	}
	rep := r.Summary.Providers[string(provider)]
	if rep == nil || rep.Aborted || rep.Error != "" || len(rep.ApplyErrors) != 0 || r.hasUnapplied(string(provider), true) {
		return false
	}
	cov := rep.Coverage
	return cov.TransactionsExhaustive && cov.TransactionsPaginatedComplete &&
		cov.TransactionWindowSince != nil && !cov.TransactionWindowSince.After(since) &&
		cov.TransactionWindowUntil != nil && !cov.TransactionWindowUntil.Before(until)
}

func (r *RunResult) hasUnapplied(provider string, financialOnly bool) bool {
	for _, finding := range r.Findings {
		if string(finding.Provider) != provider || (finding.Status != FindingStatusReconcileRequired && finding.Status != FindingStatusRequiresReview) {
			continue
		}
		if financialOnly {
			switch finding.Type {
			case FindingLocalActiveRemoteDead, FindingStatusMismatch, FindingPaymentMethodMismatch, FindingEvidenceStale:
				// These remain visible for the separate lifecycle/destructive
				// policy. An intentionally held cancellation is not a missing
				// financial receipt and must not deadlock catching up.
				continue
			}
		}
		return true
	}
	return false
}

// MarkReconciledSourceDomains flips the §3.2 confirmed-absence gate
// (billing.reconciliation_state) for every source domain the given pull
// proofs actually PROVE, and returns the domains flipped. The rule (#665):
//
//   - Absence proofs need EXHAUSTIVE coverage (SnapshotCoverage), never mere
//     event-window watermark freshness.
//   - A domain is proven only when EVERY rail in the merchant's declared
//     psps catalog was covered — and only rails with exactly
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
// Callers run it merchant-scoped after mirror writes were applied
// (enforce insert+overwrite) — an advisory dry-run proves nothing about the
// LOCAL mirror.
func MarkReconciledSourceDomains(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, proofs PullProofs) ([]string, error) {
	accounts, err := q.ListPSPsForMerchant(ctx, merchantID)
	if err != nil {
		return nil, fmt.Errorf("reconcile: list PSPs: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	byRail := map[string][]gen.BillingPsp{}
	for i := range accounts {
		byRail[accounts[i].Rail] = append(byRail[accounts[i].Rail], accounts[i])
	}

	subsProven, paymentsProven := true, true
	for rail, accts := range byRail {
		proof, ok := proofs[Provider(rail)]
		if !ok || len(accts) != 1 ||
			(proof.PspID != "" && proof.PspID != accts[0].ID.String()) {
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
		if _, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: domain, FullyReconciled: true,
		}); err != nil {
			return flipped, fmt.Errorf("reconcile: mark %s reconciled: %w", domain, err)
		}
		flipped = append(flipped, domain)
	}
	return flipped, nil
}
