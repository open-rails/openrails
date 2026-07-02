package reconcile

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #665: per-subscription provider probes — the #367 liveness worker's probing
// capability rebuilt as SNAPSHOT SOURCES for the one decision core
// (ResolveUnknownFromSnapshot). A probe answers provider truth for a single
// subscription as a narrow RemoteSnapshot; it never decides outcomes itself.
// Read-only by construction: NMI query.php + v5 GET, Stripe GET.

// ProbeSubject identifies one `unknown` subscription for a per-sub probe.
type ProbeSubject struct {
	// LocalID is the OpenRails subscription id — stamped as the NMI order
	// reference (orderid/ponumber) at signup and inherited by every rebill,
	// which is what makes per-period charge lookups possible.
	LocalID uuid.UUID
	// RailSubscriptionID is the provider-side handle (required).
	RailSubscriptionID string
	// PeriodEnd bounds the charge probe; nil (legacy import with no local
	// period evidence) skips the charge lookup — the roster read alone answers.
	PeriodEnd *time.Time
}

// SubscriptionProber resolves provider truth for ONE subscription when the
// windowed bulk fetch could not cover it (NULL period end, evidence outside the
// window, non-exhaustive roster). Implementations MUST be read-only.
type SubscriptionProber interface {
	ProbeSubscription(ctx context.Context, subj ProbeSubject) (*RemoteSnapshot, error)
}

// NMISubscriptionProber probes one NMI subscription: the period's sale actions
// by order reference (query.php) plus the remote recurring record (v5 GET).
type NMISubscriptionProber struct {
	Client *nmi.NMIClient
}

func (p *NMISubscriptionProber) ProbeSubscription(ctx context.Context, subj ProbeSubject) (*RemoteSnapshot, error) {
	if p.Client == nil {
		return nil, errors.New("nmi client not configured")
	}
	if strings.TrimSpace(subj.RailSubscriptionID) == "" {
		return nil, errors.New("rail subscription id is required")
	}
	now := time.Now().UTC()
	snap := &RemoteSnapshot{
		Provider:  ProviderNMI,
		FetchedAt: now,
		// Per-subscription scope: the v5 GET's 404 IS authoritative absence at
		// NMI (cancelled records are deleted), so this one-row roster is
		// exhaustive FOR THIS SUBJECT.
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
		Capabilities: Capabilities{Subscriptions: true, Transactions: subj.PeriodEnd != nil},
	}

	if subj.PeriodEnd != nil {
		since := subj.PeriodEnd.UTC().Add(-renewalAlignmentSlack)
		probe, err := p.Client.ProbeSalesByOrderID(subj.LocalID.String(), since)
		if err != nil {
			return nil, err
		}
		snap.Transactions = probeSaleTransactions(probe, subj.RailSubscriptionID, since)
	}

	liveness, err := p.Client.GetRecurringLiveness(subj.RailSubscriptionID)
	if err != nil {
		return nil, err
	}
	if liveness.Found {
		sub := RemoteSubscription{RailSubscriptionID: subj.RailSubscriptionID, Status: SubscriptionStatusUnknown}
		if !liveness.NextChargeDate.IsZero() {
			next := liveness.NextChargeDate
			sub.NextBillingAt = &next
			// Same convention as the bulk NMI fetcher: a next charge before
			// today's boundary means the record stalled (past_due).
			if next.Before(now.Truncate(24 * time.Hour)) {
				sub.Status = SubscriptionStatusPastDue
			} else {
				sub.Status = SubscriptionStatusActive
			}
		}
		snap.Subscriptions = []RemoteSubscription{sub}
	}
	return snap, nil
}

// probeSaleTransactions maps an order-reference sale probe onto snapshot
// transactions. NMI's query response is server-filtered to actions >= since, so
// an action with an unparseable date is floored to `since` — a provider-proven
// lower bound, not a fabricated instant.
func probeSaleTransactions(probe nmi.SaleProbeResult, railSubID string, since time.Time) []RemoteTransaction {
	floored := func(at time.Time) time.Time {
		if at.IsZero() {
			return since
		}
		return at
	}
	var out []RemoteTransaction
	if probe.SuccessFound && probe.SuccessTransactionID != "" {
		amount, _ := parseAmountCents(probe.SuccessAmount)
		out = append(out, RemoteTransaction{
			TransactionID:  probe.SuccessTransactionID,
			SubscriptionID: railSubID,
			Type:           TransactionTypeSale,
			Success:        true,
			AmountCents:    amount,
			Currency:       probe.SuccessCurrency,
			OccurredAt:     floored(probe.SuccessAt),
		})
	}
	if probe.DeclineFound {
		amount, _ := parseAmountCents(probe.DeclineAmount)
		out = append(out, RemoteTransaction{
			TransactionID:  probe.DeclineTransactionID,
			SubscriptionID: railSubID,
			Type:           TransactionTypeDecline,
			Success:        false,
			AmountCents:    amount,
			Currency:       probe.DeclineCurrency,
			OccurredAt:     floored(probe.DeclineAt),
			DeclineReason:  probe.DeclineReason,
		})
	}
	return out
}

// StripeSubscriptionProber wraps the per-subscription Stripe read
// (GET /v1/subscriptions/{id}?expand[]=latest_invoice) as a snapshot source.
type StripeSubscriptionProber struct {
	Prober subscriptions.StripeLivenessProber
}

func (p *StripeSubscriptionProber) ProbeSubscription(ctx context.Context, subj ProbeSubject) (*RemoteSnapshot, error) {
	if p.Prober == nil {
		return nil, errors.New("stripe prober not configured")
	}
	if strings.TrimSpace(subj.RailSubscriptionID) == "" {
		return nil, errors.New("rail subscription id is required")
	}
	rec, err := p.Prober.ProbeSubscription(ctx, subj.RailSubscriptionID)
	if err != nil {
		return nil, err
	}
	snap := &RemoteSnapshot{
		Provider:  ProviderStripe,
		FetchedAt: time.Now().UTC(),
		// A 404 is Stripe's authoritative "gone" — exhaustive for this subject.
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
	}
	if !rec.Found {
		return snap, nil
	}
	sub := RemoteSubscription{
		RailSubscriptionID: subj.RailSubscriptionID,
		Status:             normalizeStripeStatus(rec.Status),
		RawStatus:          rec.Status,
	}
	if !rec.CurrentPeriodEnd.IsZero() {
		end := rec.CurrentPeriodEnd
		sub.NextBillingAt = &end
	}
	snap.Subscriptions = []RemoteSubscription{sub}
	// The latest PAID invoice is charge evidence; it bills at the period start
	// it opens — the deterministic timestamp Stripe exposes here.
	if rec.LatestInvoicePaid && rec.LatestInvoiceTransactionID != "" && !rec.CurrentPeriodStart.IsZero() {
		snap.Transactions = []RemoteTransaction{{
			TransactionID:  rec.LatestInvoiceTransactionID,
			SubscriptionID: subj.RailSubscriptionID,
			Type:           TransactionTypeSale,
			Success:        true,
			AmountCents:    rec.LatestInvoiceAmountPaid,
			Currency:       rec.LatestInvoiceCurrency,
			OccurredAt:     rec.CurrentPeriodStart,
		}}
	}
	return snap, nil
}

// BuildSubscriptionProbers assembles the per-rail probe sources from the same
// runtime clients the bulk fetchers use. Best-effort: a rail without usable
// credentials simply has no probe fallback. CCBill has no per-record read API
// (its DataLink bulk lane is its prober) and Solana is pull-based — neither
// gets one, matching the retired #367 worker's rail coverage.
func BuildSubscriptionProbers(rails config.RailMerchantAccountSet, nmiClients map[string]*nmi.NMIClient) map[Provider]SubscriptionProber {
	probers := map[Provider]SubscriptionProber{}
	if _, c, err := selectNMIClient(rails, nmiClients, ""); err == nil && c != nil {
		probers[ProviderNMI] = &NMISubscriptionProber{Client: c}
	}
	if p, err := subscriptions.NewStripeLivenessProber(rails); err == nil && p != nil {
		probers[ProviderStripe] = &StripeSubscriptionProber{Prober: p}
	}
	return probers
}
