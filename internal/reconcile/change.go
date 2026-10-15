package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Change is a proposal's change as persisted in reconciliation_findings.change:
// the wire FindingChange plus what approving it executes. internal/proposals is
// its only executor.
type Change struct {
	billing.FindingChange
	Apply ChangeApply `json:"apply"`
}

// ChangeApply is what approving a change executes; exactly one field is set.
type ChangeApply struct {
	Materialize        *MaterializeSubscriptionAction `json:"materialize,omitempty"`
	BackfillPayment    *BackfillPaymentAction         `json:"backfill_payment,omitempty"`
	RecordRefund       *RecordRefundAction            `json:"record_refund,omitempty"`
	Decide             *DecideAction                  `json:"decide,omitempty"`
	AdoptPaymentMethod *AdoptPaymentMethodAction      `json:"adopt_payment_method,omitempty"`
	RevokeAccess       *RevokeAccessAction            `json:"revoke_access,omitempty"`
	RefundCharge       *RefundChargeAction            `json:"refund_charge,omitempty"`
	CancelSchedule     *CancelScheduleAction          `json:"cancel_schedule,omitempty"`
	CollectRenewal     *CollectRenewalAction          `json:"collect_renewal,omitempty"`
}

// RevokeAccessAction revokes a subscription's live access.
type RevokeAccessAction struct {
	SubscriptionID uuid.UUID
	CustomerID     uuid.UUID
}

// RefundChargeAction queues the full refund of a recorded charge of a
// canceled subscription.
type RefundChargeAction struct {
	SubscriptionID uuid.UUID
	Rail           string
	TransactionID  string
}

// CancelScheduleAction queues the provider cancel of a subscription's
// schedule.
type CancelScheduleAction struct {
	SubscriptionID uuid.UUID
}

// CollectRenewalAction queues the collection of a skipped renewal.
type CollectRenewalAction struct {
	SubscriptionID uuid.UUID
}

// The pull.* types that carry only a proposal.
const (
	// FindingChargeAfterCancel: a provider charge landed after OpenRails
	// canceled its subscription for good; approving refunds it.
	FindingChargeAfterCancel FindingType = "pull.charge.after_cancel"
	// FindingOrphanedSchedule: an OpenRails-owned provider schedule still
	// bills a subscription that ended; approving cancels it at the provider.
	FindingOrphanedSchedule FindingType = "pull.subscription.orphaned_schedule"
	// FindingRenewalSkipped: the provider skipped a renewal OpenRails
	// expected; approving collects it.
	FindingRenewalSkipped FindingType = "pull.renewal.skipped"
)

// EvidenceStaleAfter: a change that ends access is superseded rather than
// applied once its evidence is older than this.
const EvidenceStaleAfter = 7 * 24 * time.Hour

// Digest is sha256 over the change's kind and provider evidence: a dismissed
// proposal is raised again only when a read yields another digest.
func (c *Change) Digest() string {
	b, err := json.Marshal(struct {
		Kind     billing.ChangeKind `json:"kind"`
		Provider map[string]any     `json:"provider"`
	}{c.Kind, c.Evidence.Provider})
	if err != nil {
		b = []byte(string(c.Kind))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BasisSubscription is the subscription whose row version a change's basis
// names.
func (c *Change) BasisSubscription() (uuid.UUID, error) {
	if c.Apply.RefundCharge != nil {
		return c.Apply.RefundCharge.SubscriptionID, nil
	}
	if c.Subject.Type != "subscription" {
		return uuid.Nil, fmt.Errorf("a %s change names %s, not a subscription", c.Kind, c.Subject.Type)
	}
	id, err := billing.ParseSubscriptionID(c.Subject.ID)
	if err != nil {
		return uuid.Nil, err
	}
	return id.UUID(), nil
}

// DecodeChange reads a persisted change.
func DecodeChange(raw []byte) (*Change, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var c Change
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode change: %w", err)
	}
	return &c, nil
}

// MarshalText spells a transition by name, so a persisted decision survives
// a reordering of the enum.
func (k TransitionKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *TransitionKind) UnmarshalText(b []byte) error {
	for _, v := range []TransitionKind{TransitionNone, TransitionRenew, TransitionAdoptPeriodEnd, TransitionPastDue, TransitionParkUnknown, TransitionCancel} {
		if v.String() == string(b) {
			*k = v
			return nil
		}
	}
	return fmt.Errorf("unknown transition %q", b)
}

// changeBuilder turns one diff's findings into proposals.
type changeBuilder struct {
	idx        *localIndex
	pms        map[uuid.UUID]*LocalPaymentMethod
	prices     map[uuid.UUID]billing.PriceID
	observedAt time.Time
	read       string
}

func newChangeBuilder(idx *localIndex, local *LocalState, observedAt time.Time) *changeBuilder {
	b := &changeBuilder{idx: idx, pms: map[uuid.UUID]*LocalPaymentMethod{}, prices: local.PriceIDs, observedAt: observedAt.UTC(), read: "roster"}
	for i := range local.PaymentMethods {
		b.pms[local.PaymentMethods[i].ID] = &local.PaymentMethods[i]
	}
	return b
}

func (b *changeBuilder) change(f *Finding, kind billing.ChangeKind, subject billing.ChangeSubject, customer uuid.UUID) *Change {
	c := &Change{FindingChange: billing.FindingChange{
		Kind: kind, Subject: subject, From: map[string]any{}, To: map[string]any{},
		ObservedAt: b.observedAt,
		Evidence:   billing.ChangeEvidence{Read: b.read, Provider: f.RemoteEvidence},
	}}
	if c.Evidence.Provider == nil {
		c.Evidence.Provider = map[string]any{}
	}
	if customer != uuid.Nil {
		id := billing.CustomerID(customer)
		c.CustomerID = &id
	}
	return c
}

func subscriptionSubject(id uuid.UUID) billing.ChangeSubject {
	return billing.ChangeSubject{Type: "subscription", ID: billing.SubscriptionID(id).String()}
}

func transactionSubject(id string) billing.ChangeSubject {
	return billing.ChangeSubject{Type: "transaction", ID: id}
}

func (b *changeBuilder) subscriptionFrom(c *Change, s *LocalSubscription) {
	rv := s.RowVersion
	c.Basis.RowVersion = &rv
	c.From["status"] = s.Status
	c.From["current_period_ends_at"] = timeOrNil(s.CurrentPeriodEndsAt)
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func microsString(cents int64, micros *int64) string {
	if micros != nil {
		return strconv.FormatInt(*micros, 10)
	}
	return strconv.FormatInt(int64(moneyutil.CentsToMicros(moneyutil.Cents(cents))), 10)
}

// attach sets the proposal on every finding whose difference approving can
// apply; a finding without one is resolved only by dismissing it.
func (b *changeBuilder) attach(findings []Finding) {
	for i := range findings {
		f := &findings[i]
		if f.Change != nil || f.Apply == nil {
			continue
		}
		f.Change = b.fromApply(f)
	}
}

// fromApply is the proposal of a finding the diff built an apply action for.
func (b *changeBuilder) fromApply(f *Finding) *Change {
	a := f.Apply
	switch {
	case a.Materialize != nil:
		return b.createSubscription(f, a.Materialize)
	case a.BackfillPayment != nil:
		p := *a.BackfillPayment
		c := b.change(f, billing.ChangeRecordCharge, transactionSubject(p.TransactionID), p.CustomerID)
		absent := p.TransactionID
		c.Basis.AbsentRailID = &absent
		payment := map[string]any{"transaction_id": p.TransactionID, "amount": microsString(p.AmountCents, p.AmountMicros),
			"currency": p.Currency, "purchased_at": p.PurchasedAt.UTC().Format(time.RFC3339)}
		if price := b.priceID(f, p.PriceID); price != "" {
			payment["price_id"] = price
		}
		if p.SubscriptionID != nil {
			payment["subscription_id"] = billing.SubscriptionID(*p.SubscriptionID).String()
		}
		c.To["payment"] = payment
		c.To["grants_access"] = p.Grant != nil
		c.Apply.BackfillPayment = &p
		return c
	case a.RecordRefund != nil:
		r := *a.RecordRefund
		amount, err := moneyutil.RailMinorToNative(r.Currency, moneyutil.Cents(r.AmountCents))
		if err != nil {
			return nil
		}
		c := b.change(f, billing.ChangeRecordRefund, transactionSubject(r.TransactionID), r.CustomerID)
		if !r.MarkRefundedOnly {
			absent := r.TransactionID
			c.Basis.AbsentRailID = &absent
		}
		refund := map[string]any{"transaction_id": r.TransactionID, "amount": strconv.FormatInt(amount, 10), "currency": r.Currency}
		if r.RefundedPaymentID != nil {
			refund["refunded_payment_id"] = billing.PaymentID(*r.RefundedPaymentID).String()
		}
		c.To["refund"] = refund
		c.Apply.RecordRefund = &r
		return c
	case a.Decide != nil:
		return b.decide(f, a.Decide)
	case a.AdoptPaymentMethod != nil:
		ap := *a.AdoptPaymentMethod
		pm := b.pms[ap.PaymentMethodID]
		var customer uuid.UUID
		if pm != nil {
			customer = pm.CustomerID
		}
		c := b.change(f, billing.ChangeUpdateCard, billing.ChangeSubject{Type: "payment_method", ID: billing.PaymentMethodID(ap.PaymentMethodID).String()}, customer)
		if pm != nil {
			c.From["card"] = pm.Card.Details()
			version := pm.CardVersionID.String()
			if pm.CardVersionID == uuid.Nil {
				version = ""
			}
			c.Basis.CardVersionID = &version
		}
		c.To["card"] = ap.Card.Details()
		c.Apply.AdoptPaymentMethod = &ap
		return c
	}
	return nil
}

// priceID is a price's public ID: from the local state, or the public ID the
// diff recorded in the finding's local evidence (a Solana memo purchase).
func (b *changeBuilder) priceID(f *Finding, id uuid.UUID) string {
	if public, ok := b.prices[id]; ok {
		return public.String()
	}
	public, _ := f.LocalEvidence["price_id"].(string)
	return public
}

func (b *changeBuilder) createSubscription(f *Finding, m *MaterializeSubscriptionAction) *Change {
	subject := billing.ChangeSubject{Type: "provider_subscription", ID: m.RailSubscriptionID}
	c := b.change(f, billing.ChangeCreateSubscription, subject, m.CustomerID)
	absent := m.RailSubscriptionID
	c.Basis.AbsentRailID = &absent
	c.From = nil
	c.To["status"] = string(m.Status)
	if price := b.priceID(f, m.PriceID); price != "" {
		c.To["price_id"] = price
	}
	c.To["current_period_starts_at"] = timeOrNil(m.PeriodStartsAt)
	c.To["current_period_ends_at"] = timeOrNil(m.PeriodEndsAt)
	if m.Backfill != nil {
		c.To["latest_charge"] = map[string]any{"transaction_id": m.Backfill.TransactionID,
			"amount": microsString(m.Backfill.AmountCents, m.Backfill.AmountMicros), "currency": m.Backfill.Currency}
	}
	mc := *m
	if m.Backfill != nil {
		backfill := *m.Backfill
		mc.Backfill = &backfill
	}
	c.Apply.Materialize = &mc
	return c
}

// decide maps a decider transition: a cancel ends access; a renewal, an
// adopted period end, past due and a park are adoptions.
func (b *changeBuilder) decide(f *Finding, d *DecideAction) *Change {
	s := b.idx.byID[d.SubscriptionID]
	var customer uuid.UUID
	if s != nil {
		customer = s.CustomerID
	}
	kind := billing.ChangeAdoptSubscription
	if d.Decision.Kind == TransitionCancel {
		kind = billing.ChangeCancelSubscription
	}
	c := b.change(f, kind, subscriptionSubject(d.SubscriptionID), customer)
	if s != nil {
		b.subscriptionFrom(c, s)
	}
	c.To["transition"] = d.Decision.Kind.String()
	switch d.Decision.Kind {
	case TransitionCancel:
		c.Effects.EndsAccess = true
		c.To["status"] = "canceled"
		ended := d.Decision.EvidenceAt
		if ended.IsZero() {
			ended = b.observedAt
		}
		c.To["ended_at"] = ended.UTC().Format(time.RFC3339)
	case TransitionRenew:
		c.To["status"] = "active"
		c.To["current_period_ends_at"] = timeOrNil(d.Decision.NewPeriodEnd)
	case TransitionAdoptPeriodEnd:
		c.To["status"] = "active"
		c.To["current_period_ends_at"] = timeOrNil(d.Decision.NewPeriodEnd)
	case TransitionPastDue:
		c.To["status"] = "past_due"
	case TransitionParkUnknown:
		c.To["status"] = "unverified"
	}
	if d.Decision.Reason != "" {
		c.To["reason"] = d.Decision.Reason
	}
	dc := *d
	c.Apply.Decide = &dc
	return c
}

// revokeAccess proposes revoking a live subscription's access.
func (b *changeBuilder) revokeAccess(f *Finding, s *LocalSubscription) *Change {
	c := b.change(f, billing.ChangeRevokeAccess, subscriptionSubject(s.ID), s.CustomerID)
	b.subscriptionFrom(c, s)
	c.Effects.EndsAccess = true
	c.To["access"] = "revoked"
	c.Apply.RevokeAccess = &RevokeAccessAction{SubscriptionID: s.ID, CustomerID: s.CustomerID}
	return c
}

// refundAfterCancel proposes the full refund of a charge that landed after
// its subscription was canceled.
func (b *changeBuilder) refundAfterCancel(f *Finding, s *LocalSubscription, p *BackfillPaymentAction) *Change {
	c := b.change(f, billing.ChangeRefundChargeAfterCancel, transactionSubject(p.TransactionID), s.CustomerID)
	b.subscriptionFrom(c, s)
	c.Effects.MovesMoney = true
	c.To["refund"] = map[string]any{"transaction_id": p.TransactionID, "amount": microsString(p.AmountCents, p.AmountMicros), "currency": p.Currency}
	c.Apply.RefundCharge = &RefundChargeAction{SubscriptionID: s.ID, Rail: p.Rail, TransactionID: p.TransactionID}
	return c
}

// cancelSchedule proposes the provider cancel of an ended subscription's
// schedule.
func (b *changeBuilder) cancelSchedule(f *Finding, s *LocalSubscription) *Change {
	c := b.change(f, billing.ChangeCancelProviderSchedule, subscriptionSubject(s.ID), s.CustomerID)
	b.subscriptionFrom(c, s)
	c.Effects.ProviderWrite = true
	c.To["provider_schedule"] = "canceled"
	c.Apply.CancelSchedule = &CancelScheduleAction{SubscriptionID: s.ID}
	return c
}
