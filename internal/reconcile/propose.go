package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ProposeParams is one page of a PSP import's reconcile: what its pull read
// of the objects OpenRails holds, against the book of Customers.
type ProposeParams struct {
	Binding  PSPBinding
	Snapshot *RemoteSnapshot
	Coverage PSPCoverage
	// Basis is each subscription's row version before the roster read. A
	// state change is proposed only for a row still at it; a row created
	// since is never treated as absent.
	Basis map[uuid.UUID]int64
	// Customers bounds the local book read; nil reads the PSP's whole book.
	Customers []uuid.UUID
	// Excluded are provider subscriptions held by one customer and linked to
	// another: nothing is proposed about them until a person links them.
	Excluded map[string]bool
	// PullID tags every proposal; RunID is the reconcile run they are seen in.
	PullID, RunID uuid.UUID
	Contacts      identity.Directory
	Now           time.Time
}

// ProposeResult is what one page proposed.
type ProposeResult struct {
	Proposed int
	// Moved are the provider ids of basis subscriptions whose row changed
	// since the roster read: no state change is proposed for them.
	Moved []string
}

// Propose diffs one page of a PSP's local book against what a pull read and
// raises every difference as a pull.* proposal of the pull. It writes nothing
// else: a proposal applies only when approved through internal/proposals.
func Propose(ctx context.Context, d *db.DB, p ProposeParams) (ProposeResult, error) {
	var out ProposeResult
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if p.Binding.ID == uuid.Nil || p.Snapshot == nil || p.PullID == uuid.Nil || p.RunID == uuid.Nil {
		return out, errors.New("reconcile: a proposal pass names its PSP, snapshot, pull and run")
	}
	provider := Provider(p.Binding.Rail)
	snap := p.Snapshot
	snap.PspID = p.Binding.ID.String()
	if !p.Coverage.Complete() {
		snap.Coverage.SubscriptionsExhaustive = false
	}
	loader := &PGLocalStateLoader{DB: d, Contacts: p.Contacts}
	local, err := loader.LoadCustomers(ctx, provider, p.Binding.ID, p.Customers)
	if err != nil {
		return out, fmt.Errorf("load local state: %w", err)
	}
	if len(p.Excluded) > 0 {
		local.Subscriptions = slices.DeleteFunc(local.Subscriptions, func(s LocalSubscription) bool { return p.Excluded[s.RailSubscriptionID] })
		snap.Subscriptions = slices.DeleteFunc(snap.Subscriptions, func(r RemoteSubscription) bool { return p.Excluded[r.RailSubscriptionID] })
	}
	txnIDs := collectTxnLookupIDs(snap)
	payments, err := loader.PaymentsByTransactionIDs(ctx, provider, p.Binding.ID, txnIDs)
	if err != nil {
		return out, fmt.Errorf("load local payments: %w", err)
	}
	if len(txnIDs) > 0 {
		if local.Attributions, err = loader.ChargeAttributions(ctx, p.Binding.ID, txnIDs); err != nil {
			return out, fmt.Errorf("load charge attributions: %w", err)
		}
	}
	now := p.Now.UTC()
	findings := diffProvider(provider, snap, local, payments, now, diffOptions{EvidenceFloor: EvidenceFloorFor(ctx, d, mid.UUID()), AwaitUnverified: true})
	bindApplyActions(findings, p.Binding.ID, p.RunID)

	subs := make(map[uuid.UUID]*LocalSubscription, len(local.Subscriptions))
	for i := range local.Subscriptions {
		subs[local.Subscriptions[i].ID] = &local.Subscriptions[i]
	}
	moved := map[uuid.UUID]bool{}
	store := &PGStore{DB: d, Now: func() time.Time { return now }}
	for i := range findings {
		f := &findings[i]
		if !strings.HasPrefix(string(f.Type), "pull.") || f.Type == FindingRemoteSubMissingLocal {
			continue
		}
		if id, state := stateSubject(f, subs); state {
			version, ok := p.Basis[id]
			if !ok {
				continue // created after the roster read
			}
			if version != subs[id].RowVersion {
				if !moved[id] {
					moved[id] = true
					out.Moved = append(out.Moved, subs[id].RailSubscriptionID)
				}
				continue
			}
		}
		f.PSPID, f.Source, f.PullID = p.Binding.ID, billing.ProposalSourcePull, p.PullID
		if _, err := store.UpsertFinding(ctx, p.RunID, *f); err != nil {
			return out, fmt.Errorf("persist proposal %s/%s: %w", f.Type, f.SubjectKey, err)
		}
		out.Proposed++
	}
	return out, nil
}

// stateSubject is the local subscription whose state a finding would change.
// Money facts (a charge or refund to record) name none: they need no basis.
func stateSubject(f *Finding, subs map[uuid.UUID]*LocalSubscription) (uuid.UUID, bool) {
	if c := f.Change; c != nil {
		if c.Basis.RowVersion == nil {
			return uuid.Nil, false
		}
		id, err := c.BasisSubscription()
		return id, err == nil && subs[id] != nil
	}
	switch f.Type {
	case FindingChargeMissingLocal, FindingRefundUnrecorded, FindingChargebackActiveSub, FindingReversalUnlinked:
		return uuid.Nil, false
	}
	if f.Apply != nil && f.Apply.Decide != nil {
		return f.Apply.Decide.SubscriptionID, subs[f.Apply.Decide.SubscriptionID] != nil
	}
	id, err := uuid.Parse(f.SubjectKey)
	return id, err == nil && subs[id] != nil
}

// CorrectSubscriptionAction adopts the provider's facts on a held
// subscription: the price its plan maps to, the recurring record its
// schedule bills by, and the date it started. Unset fields stand.
type CorrectSubscriptionAction struct {
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	PriceID        *uuid.UUID `json:"price_id,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	// Schedule is the provider's recurring record, as the pull read it.
	Schedule *RemoteSubscription `json:"schedule,omitempty"`
}

// mappedPrice is the one local price a provider plan maps to, nil when none
// or several do.
func mappedPrice(planIdx map[string][]planLink, plan string) *LocalPrice {
	if links := planIdx[strings.TrimSpace(plan)]; len(links) == 1 && strings.TrimSpace(plan) != "" {
		return links[0].price
	}
	return nil
}

// compareFacts proposes adopting the provider's facts on a held provider-run
// subscription (pull.subscription.facts, correct_subscription): the price its
// plan maps to (on Stripe once an invoice for it is paid), the schedule terms
// of its recurring record in place of a derived or missing one, and its start
// date. A price of another product ends the old product's access. Unmapped
// plans, vault moves and pauses stay compareScheduleTerms' drift.
func compareFacts(provider Provider, snap *RemoteSnapshot, idx *localIndex, planIdx map[string][]planLink, s *LocalSubscription, r *RemoteSubscription, opts diffOptions) *Finding {
	if opts.changes == nil || s.PriceID == nil || s.CollectionPolicy == models.CollectionPolicyEngine || s.TierChangePending || r.Paused {
		return nil
	}
	live := (s.IsLive() || s.Status == string(models.StatusUnverified)) && remoteLive(r.Status)
	act := &CorrectSubscriptionAction{SubscriptionID: s.ID}
	from, to := map[string]any{}, map[string]any{}
	billed := localPrice(idx, *s.PriceID)
	endsAccess := false
	if target := mappedPrice(planIdx, r.PlanID); live && target != nil && target.ID != *s.PriceID &&
		(s.ScheduledPriceID == nil || *s.ScheduledPriceID != target.ID) &&
		(provider != ProviderStripe || paidStripeInvoice(snap, r.RailSubscriptionID, r.PlanID)) {
		id := target.ID
		act.PriceID, billed = &id, target
		from["price_id"], to["price_id"] = opts.changes.prices[*s.PriceID].String(), opts.changes.prices[target.ID].String()
		endsAccess = target.ProductID != s.ProductID
	}
	if live && (provider == ProviderNMI || provider == ProviderCCBill) && billed != nil && r.NextBillingAt != nil &&
		s.AcceptedScheduleSource != providerScheduleSource(models.Rail(s.Rail), s.RailSubscriptionID) && localTermsAgree(r, billed) {
		schedule := *r
		schedule.Raw = nil
		act.Schedule = &schedule
		from["schedule"] = nil
		if s.AcceptedScheduleSource != "" {
			from["schedule"] = s.AcceptedScheduleSource
		}
		to["schedule"] = map[string]any{"source": providerScheduleSource(models.Rail(s.Rail), s.RailSubscriptionID), "amount": r.Amount,
			"interval_days": r.IntervalDays, "interval_months": r.IntervalMonths, "first_due_at": timeOrNil(r.NextBillingAt)}
	}
	if !r.CreatedAt.IsZero() && !r.CreatedAt.UTC().Truncate(24*time.Hour).Equal(s.StartedAt.UTC().Truncate(24*time.Hour)) {
		started := r.CreatedAt.UTC()
		act.StartedAt = &started
		from["started_at"], to["started_at"] = s.StartedAt.UTC().Format(time.RFC3339), started.Format(time.RFC3339)
	}
	if act.PriceID == nil && act.Schedule == nil && act.StartedAt == nil {
		return nil
	}
	remote := remoteSubEvidence(r)
	remote["amount"], remote["interval_days"], remote["interval_months"] = r.Amount, r.IntervalDays, r.IntervalMonths
	if !r.CreatedAt.IsZero() {
		remote["started_at"] = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	f := Finding{
		Provider:          provider,
		Type:              FindingSubscriptionFacts,
		SubjectKey:        s.ID.String(),
		Severity:          SeverityMedium,
		Status:            FindingStatusReconcileRequired,
		LocalEvidence:     localSubEvidence(s),
		RemoteEvidence:    remote,
		RecommendedAction: "the provider bills this subscription on other facts than OpenRails records (" + strings.Join(driftFields(to), ", ") + "); approving adopts the provider's",
	}
	c := opts.changes.change(&f, billing.ChangeCorrectSubscription, subscriptionSubject(s.ID), s.CustomerID)
	opts.changes.subscriptionFrom(c, s)
	for k, v := range from {
		c.From[k] = v
	}
	for k, v := range to {
		c.To[k] = v
	}
	c.Effects.EndsAccess = endsAccess
	if endsAccess {
		f.Severity = SeverityHigh
	}
	c.Apply.Correct = act
	f.Change = c
	return &f
}

func localPrice(idx *localIndex, id uuid.UUID) *LocalPrice {
	for i := range idx.prices {
		if idx.prices[i].ID == id {
			return &idx.prices[i]
		}
	}
	return nil
}

// localTermsAgree reports a recurring record billing the price's amount at
// its cadence: the terms a schedule on it may adopt.
func localTermsAgree(r *RemoteSubscription, p *LocalPrice) bool {
	switch {
	case r.Amount == "":
		return false
	case p.BillingIntervalMonths != nil:
		if r.IntervalMonths != *p.BillingIntervalMonths || r.IntervalDays != 0 {
			return false
		}
	case p.BillingCycleDays == nil || *p.BillingCycleDays <= 0 || r.IntervalMonths != 0 || r.IntervalDays != *p.BillingCycleDays:
		return false
	}
	currency := moneyutil.NormalizeCurrency(p.Currency)
	minor, err := moneyutil.DecimalToRailMinor(currency, r.Amount)
	if err != nil {
		return false
	}
	amount, err := moneyutil.RailMinorToNative(currency, minor)
	return err == nil && amount == p.Amount
}

// paidStripeInvoice reports a paid invoice of the subscription billing the
// price: a Stripe price move counts only then.
func paidStripeInvoice(snap *RemoteSnapshot, subscriptionID, price string) bool {
	for i := range snap.Transactions {
		bill := snap.Transactions[i].StripeBill
		if bill == nil || bill.SubscriptionID != subscriptionID || bill.Status != "paid" {
			continue
		}
		for _, line := range bill.Lines {
			if line.PriceID == price {
				return true
			}
		}
	}
	return false
}

// ErrScheduleTerms is a recurring record a correction can no longer adopt.
var ErrScheduleTerms = errors.New("the provider's recurring record does not bill the subscription's price")

// AdoptProviderSchedule accepts the provider's recurring record as sub's
// schedule on price, under the source its provider names it by. It runs in
// d's transaction.
func AdoptProviderSchedule(ctx context.Context, d *db.DB, sub *models.Subscription, price gen.BillingPrice, record RemoteSubscription, at time.Time) (string, error) {
	amount, hours, months, reason := providerTerms(&record, price, sub.Quantity)
	if reason != "" {
		return "", fmt.Errorf("%w: %s", ErrScheduleTerms, reason)
	}
	bill, err := scheduleBill(ctx, d.Gen(ctx), sub, price, price.Currency, amount, sub.Quantity, hours)
	if err != nil {
		return "", err
	}
	source := providerScheduleSource(models.Rail(sub.Rail), sub.RailSubscriptionID)
	return source, observed.DeclareSchedule(ctx, d, source, bill, *record.NextBillingAt, hours, months, at)
}
