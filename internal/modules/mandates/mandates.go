// Package mandates owns stored-credential agreements (#1168): one agreement on
// one saved card, recurring for a subscription, unscheduled for collection in
// one currency, or card_on_file for one-click reuse. A mandate holds the
// network lineage its storing transaction established, scoped to the gateway
// account that ran it. Charges cite a mandate; they never carry references of
// their own. Every lookup is scoped by merchant and customer.
package mandates

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/pagination"
)

// End reasons.
const (
	EndReplaced             = "replaced"
	EndBrandChanged         = "brand_changed"
	EndClosed               = "closed"
	EndSubscriptionEnded    = "subscription_ended"
	EndPaymentMethodRemoved = "payment_method_removed"
)

// Statuses a mandate can charge or be re-consented from.
const (
	StatusActive            = "active"
	StatusRequiresReconsent = "requires_reconsent"
)

var (
	// ErrMissing: no mandate covers the charge on this card and account.
	ErrMissing = fmt.Errorf("no mandate covers this charge: %w", charge.ErrAgreementRequired)
	// ErrNotActive: the mandate exists but cannot authorize a charge now.
	ErrNotActive = fmt.Errorf("mandate is not active: %w", charge.ErrAgreementRequired)
	// ErrChanged: the mandate a charge froze no longer matches its row.
	ErrChanged = errors.New("mandate changed since the charge was accepted")
)

// Lineage is the mandate's references as a charge cites them.
func Lineage(m gen.BillingMandate) *charge.Mandate {
	out := &charge.Mandate{ID: m.ID, Kind: charge.Agreement(m.Kind)}
	if m.InitialTransactionID != nil {
		out.InitialTransactionID = *m.InitialTransactionID
	}
	if m.NetworkTransactionID != nil {
		out.NetworkTransactionID = *m.NetworkTransactionID
	}
	if m.TransactionLinkID != nil {
		out.TransactionLinkID = *m.TransactionLinkID
	}
	return out
}

// citableKinds are the mandates whose lineage a charge under a may cite: the
// same network sequence, or any on Stripe, whose off-session setup covers every
// later off-session use and which links merchant-initiated charges itself.
func citableKinds(rail string, a charge.Agreement) []string {
	if rail == "stripe" {
		return []string{string(charge.AgreementRecurring), string(charge.AgreementUnscheduled), string(charge.AgreementCardOnFile)}
	}
	if a.Sequence() == charge.AgreementRecurring {
		return []string{string(charge.AgreementRecurring)}
	}
	return []string{string(charge.AgreementUnscheduled), string(charge.AgreementCardOnFile)}
}

// Citable is the lineage a charge under a on this customer's card, through
// psp, cites: nil when there is none and the charge is the storing transaction.
func Citable(ctx context.Context, q *gen.Queries, merchantID, customerID, methodID, pspID uuid.UUID, rail string, a charge.Agreement) (*charge.Mandate, error) {
	if a == charge.AgreementNone {
		return nil, nil
	}
	m, err := q.GetCitableMandateForShare(ctx, gen.GetCitableMandateForShareParams{
		MerchantID: merchantID, CustomerID: customerID, PaymentMethodID: methodID, PspID: pspID, Kinds: citableKinds(rail, a),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Lineage(m), nil
}

// ForSubscription is the active recurring mandate a subscription's renewal on
// this card, through psp, charges under.
func ForSubscription(ctx context.Context, q *gen.Queries, merchantID, customerID, subscriptionID, methodID, pspID uuid.UUID) (*charge.Mandate, error) {
	m, err := q.GetLiveRecurringMandateForShare(ctx, gen.GetLiveRecurringMandateForShareParams{MerchantID: merchantID, SubscriptionID: subscriptionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	return chargeable(m, customerID, methodID, pspID)
}

// ForCollection is the active unscheduled mandate a collection in currency on
// this card, through psp, charges under.
func ForCollection(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, methodID, pspID uuid.UUID) (*charge.Mandate, error) {
	m, err := q.GetLiveUnscheduledMandateForShare(ctx, gen.GetLiveUnscheduledMandateForShareParams{MerchantID: merchantID, CustomerID: customerID, Currency: strings.ToUpper(strings.TrimSpace(currency))})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	return chargeable(m, customerID, methodID, pspID)
}

// chargeable: a merchant-initiated charge runs only under an active mandate on
// the card it charges, with references from the account it charges through.
func chargeable(m gen.BillingMandate, customerID, methodID, pspID uuid.UUID) (*charge.Mandate, error) {
	if m.CustomerID != customerID || m.PaymentMethodID == nil || *m.PaymentMethodID != methodID || m.PspID != pspID {
		return nil, fmt.Errorf("%w: mandate %s is on another card or account", ErrMissing, m.ID)
	}
	if m.Status != StatusActive {
		return nil, fmt.Errorf("%w: mandate %s is %s", ErrNotActive, m.ID, m.Status)
	}
	if m.InitialTransactionID == nil {
		return nil, fmt.Errorf("%w: mandate %s has no storing transaction", ErrMissing, m.ID)
	}
	return Lineage(m), nil
}

// Recheck confirms, under a shared lock, that a lineage a charge froze is
// still its mandate's and the mandate is active. nil cites nothing.
func Recheck(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, cited *charge.Mandate) error {
	if cited == nil {
		return nil
	}
	m, err := q.GetMandateForShare(ctx, gen.GetMandateForShareParams{MerchantID: merchantID, ID: cited.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: mandate %s is gone", ErrChanged, cited.ID)
	}
	if err != nil {
		return err
	}
	if m.Status != StatusActive {
		return fmt.Errorf("%w: mandate %s is %s", ErrNotActive, m.ID, m.Status)
	}
	if *Lineage(m) != *cited {
		return fmt.Errorf("%w: mandate %s", ErrChanged, m.ID)
	}
	return nil
}

// Agreement is one mandate to record.
type Agreement struct {
	MerchantID, CustomerID, PaymentMethodID, PSPID uuid.UUID
	Rail                                           string
	Kind                                           charge.Agreement
	// SubscriptionID scopes a recurring mandate; Currency an unscheduled one.
	SubscriptionID *uuid.UUID
	Currency       string
	// Lineage is the cited mandate's references, or the storing transaction's;
	// nil when consent comes before any storing transaction.
	Lineage    *charge.Mandate
	AcceptedAt time.Time
}

func (a Agreement) insert() (gen.InsertMandateParams, error) {
	p := gen.InsertMandateParams{
		MerchantID: a.MerchantID, CustomerID: a.CustomerID, PaymentMethodID: a.PaymentMethodID, PspID: a.PSPID,
		Rail: a.Rail, Kind: string(a.Kind), SubscriptionID: a.SubscriptionID, AcceptedAt: a.AcceptedAt.UTC(),
	}
	if a.MerchantID == uuid.Nil || a.CustomerID == uuid.Nil || a.PaymentMethodID == uuid.Nil || a.PSPID == uuid.Nil || a.Rail == "" || a.AcceptedAt.IsZero() {
		return p, errors.New("mandate: merchant, customer, card, account, rail and acceptance are required")
	}
	switch a.Kind {
	case charge.AgreementRecurring:
		if a.SubscriptionID == nil || a.Currency != "" {
			return p, errors.New("mandate: a recurring agreement covers exactly one subscription")
		}
	case charge.AgreementUnscheduled:
		if a.SubscriptionID != nil || strings.TrimSpace(a.Currency) == "" {
			return p, errors.New("mandate: an unscheduled agreement covers exactly one currency")
		}
		currency := strings.ToUpper(strings.TrimSpace(a.Currency))
		p.Currency = &currency
	case charge.AgreementCardOnFile:
		if a.SubscriptionID != nil || a.Currency != "" {
			return p, errors.New("mandate: card-on-file consent has no scope")
		}
	default:
		return p, fmt.Errorf("mandate: unknown agreement %q", a.Kind)
	}
	if l := a.Lineage; l != nil {
		p.InitialTransactionID = optional(l.InitialTransactionID)
		p.NetworkTransactionID = optional(l.NetworkTransactionID)
		p.TransactionLinkID = optional(l.TransactionLinkID)
		if p.InitialTransactionID == nil {
			return p, errors.New("mandate: a lineage needs its initial transaction id")
		}
	}
	return p, nil
}

// Create records a new active mandate.
func Create(ctx context.Context, q *gen.Queries, a Agreement) (gen.BillingMandate, error) {
	p, err := a.insert()
	if err != nil {
		return gen.BillingMandate{}, err
	}
	return q.InsertMandate(ctx, p)
}

// Replace makes a the live mandate of its scope: an equivalent live one (same
// card, account and lineage) stands; any other ends as replaced first.
func Replace(ctx context.Context, q *gen.Queries, a Agreement, now time.Time) (gen.BillingMandate, error) {
	p, err := a.insert()
	if err != nil {
		return gen.BillingMandate{}, err
	}
	live, found, err := liveInScope(ctx, q, a)
	if err != nil {
		return gen.BillingMandate{}, err
	}
	if found {
		if live.Status == StatusActive && live.PaymentMethodID != nil && *live.PaymentMethodID == a.PaymentMethodID && live.PspID == a.PSPID && sameLineage(live, a.Lineage) {
			return live, nil
		}
		reason := EndReplaced
		if live.Status == StatusRequiresReconsent {
			reason = EndBrandChanged
		}
		if _, err := q.EndMandate(ctx, gen.EndMandateParams{MerchantID: a.MerchantID, ID: live.ID, Reason: reason, Now: now.UTC()}); err != nil {
			return gen.BillingMandate{}, err
		}
	}
	return q.InsertMandate(ctx, p)
}

func liveInScope(ctx context.Context, q *gen.Queries, a Agreement) (gen.BillingMandate, bool, error) {
	var m gen.BillingMandate
	var err error
	switch a.Kind {
	case charge.AgreementRecurring:
		m, err = q.GetLiveRecurringMandateForShare(ctx, gen.GetLiveRecurringMandateForShareParams{MerchantID: a.MerchantID, SubscriptionID: *a.SubscriptionID})
	case charge.AgreementUnscheduled:
		m, err = q.GetLiveUnscheduledMandateForShare(ctx, gen.GetLiveUnscheduledMandateForShareParams{MerchantID: a.MerchantID, CustomerID: a.CustomerID, Currency: strings.ToUpper(strings.TrimSpace(a.Currency))})
	default:
		m, err = q.GetLiveCardOnFileMandate(ctx, gen.GetLiveCardOnFileMandateParams{MerchantID: a.MerchantID, PaymentMethodID: a.PaymentMethodID, PspID: a.PSPID})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	if err == nil && m.CustomerID != a.CustomerID {
		return m, false, fmt.Errorf("mandate %s belongs to another customer", m.ID)
	}
	return m, err == nil, err
}

func sameLineage(m gen.BillingMandate, l *charge.Mandate) bool {
	if l == nil {
		return m.InitialTransactionID == nil
	}
	got := Lineage(m)
	got.ID, got.Kind = uuid.Nil, ""
	want := *l
	want.ID, want.Kind = uuid.Nil, ""
	return *got == want
}

// Stored records what an approved customer-present charge in the unscheduled
// sequence established on a card: its card_on_file consent (always save), and,
// when the card is the customer's default in currency and no
// collection mandate is live there, that agreement too. A charge that cited a
// lineage records only what has none yet.
type Stored struct {
	MerchantID, CustomerID, PaymentMethodID, PSPID uuid.UUID
	Rail, Currency                                 string
	// Lineage is the cited mandate's, or the storing transaction's references.
	Lineage    charge.Mandate
	AcceptedAt time.Time
}

// RecordStored applies Stored.
func RecordStored(ctx context.Context, q *gen.Queries, s Stored) error {
	lineage := s.Lineage
	lineage.ID, lineage.Kind = uuid.Nil, ""
	if strings.TrimSpace(lineage.InitialTransactionID) == "" {
		return errors.New("mandate: a stored card needs its initial transaction id")
	}
	onFile := Agreement{MerchantID: s.MerchantID, CustomerID: s.CustomerID, PaymentMethodID: s.PaymentMethodID, PSPID: s.PSPID, Rail: s.Rail, Kind: charge.AgreementCardOnFile, Lineage: &lineage, AcceptedAt: s.AcceptedAt}
	if err := fill(ctx, q, onFile); err != nil {
		return err
	}
	// The collection agreement shares the card's standing lineage.
	if live, found, err := liveInScope(ctx, q, onFile); err != nil {
		return err
	} else if found && live.InitialTransactionID != nil {
		standing := Lineage(live)
		standing.ID, standing.Kind = uuid.Nil, ""
		onFile.Lineage = standing
	}
	currency := strings.ToUpper(strings.TrimSpace(s.Currency))
	if currency == "" {
		return nil
	}
	settings, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: s.MerchantID, CustomerID: s.CustomerID, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if settings.DefaultPaymentMethodID == nil || *settings.DefaultPaymentMethodID != s.PaymentMethodID {
		return nil
	}
	collection := onFile
	collection.Kind, collection.Currency = charge.AgreementUnscheduled, currency
	return fill(ctx, q, collection)
}

// fill creates a's scope's mandate when none is live, or gives a live one
// without references its lineage. A live one with references stands, unless
// it waits for consent: the customer's storing charge is that consent, and
// replaces it.
func fill(ctx context.Context, q *gen.Queries, a Agreement) error {
	live, found, err := liveInScope(ctx, q, a)
	if err != nil {
		return err
	}
	if found && live.Status == StatusRequiresReconsent {
		if _, err := q.EndMandate(ctx, gen.EndMandateParams{MerchantID: a.MerchantID, ID: live.ID, Reason: EndBrandChanged, Now: a.AcceptedAt.UTC()}); err != nil {
			return err
		}
		found = false
	}
	if !found {
		_, err := Create(ctx, q, a)
		return err
	}
	if live.InitialTransactionID != nil || live.PaymentMethodID == nil || *live.PaymentMethodID != a.PaymentMethodID || live.PspID != a.PSPID {
		return nil
	}
	_, err = q.SetMandateLineage(ctx, gen.SetMandateLineageParams{
		MerchantID: a.MerchantID, ID: live.ID, InitialTransactionID: a.Lineage.InitialTransactionID,
		NetworkTransactionID: optional(a.Lineage.NetworkTransactionID), TransactionLinkID: optional(a.Lineage.TransactionLinkID),
	})
	return err
}

// SetLineage gives a mandate made before its storing transaction that
// transaction's references.
func SetLineage(ctx context.Context, q *gen.Queries, merchantID, mandateID uuid.UUID, lineage charge.Mandate) error {
	n, err := q.SetMandateLineage(ctx, gen.SetMandateLineageParams{
		MerchantID: merchantID, ID: mandateID, InitialTransactionID: lineage.InitialTransactionID,
		NetworkTransactionID: optional(lineage.NetworkTransactionID), TransactionLinkID: optional(lineage.TransactionLinkID),
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: mandate %s already has references or ended", ErrChanged, mandateID)
	}
	return nil
}

// RequireReconsent holds a card's active mandates for the customer's fresh
// consent: an account updater reissued it under another brand, so no network
// lineage survives. Merchant-initiated charges under them are refused.
func RequireReconsent(ctx context.Context, q *gen.Queries, merchantID, methodID uuid.UUID, now time.Time) error {
	_, err := q.RequireMandatesReconsent(ctx, gen.RequireMandatesReconsentParams{MerchantID: merchantID, PaymentMethodID: methodID, Now: now.UTC()})
	return err
}

// AwaitingConsent are a card's mandates waiting for the customer's consent,
// locked for their replacement.
func AwaitingConsent(ctx context.Context, q *gen.Queries, merchantID, methodID uuid.UUID) ([]gen.BillingMandate, error) {
	return q.ListMandatesAwaitingConsent(ctx, gen.ListMandatesAwaitingConsentParams{MerchantID: merchantID, PaymentMethodID: methodID})
}

// Reconsent replaces a mandate waiting for consent with an active one in the
// same scope, citing the lineage the customer's verification established.
func Reconsent(ctx context.Context, q *gen.Queries, m gen.BillingMandate, lineage charge.Mandate, now time.Time) (gen.BillingMandate, error) {
	if m.Status != StatusRequiresReconsent || m.PaymentMethodID == nil {
		return gen.BillingMandate{}, fmt.Errorf("%w: mandate %s is not waiting for consent", ErrChanged, m.ID)
	}
	lineage.ID, lineage.Kind = uuid.Nil, ""
	a := Agreement{MerchantID: m.MerchantID, CustomerID: m.CustomerID, PaymentMethodID: *m.PaymentMethodID, PSPID: m.PspID, Rail: m.Rail,
		Kind: charge.Agreement(m.Kind), SubscriptionID: m.SubscriptionID, Lineage: &lineage, AcceptedAt: now}
	if m.Currency != nil {
		a.Currency = *m.Currency
	}
	return Replace(ctx, q, a, now)
}

// Reuse is the customer's consent to reuse a card for one-click buys, on the
// account psp: an existing live consent stands; a new one cites the card's
// standing unscheduled lineage there, or none until its next storing charge.
func Reuse(ctx context.Context, q *gen.Queries, merchantID, customerID, methodID, pspID uuid.UUID, rail string, now time.Time) error {
	if _, found, err := liveInScope(ctx, q, Agreement{MerchantID: merchantID, CustomerID: customerID, PaymentMethodID: methodID, PSPID: pspID, Kind: charge.AgreementCardOnFile}); err != nil || found {
		return err
	}
	lineage, err := Citable(ctx, q, merchantID, customerID, methodID, pspID, rail, charge.AgreementCardOnFile)
	if err != nil {
		return err
	}
	if lineage != nil {
		lineage.ID, lineage.Kind = uuid.Nil, ""
	}
	_, err = Create(ctx, q, Agreement{MerchantID: merchantID, CustomerID: customerID, PaymentMethodID: methodID, PSPID: pspID, Rail: rail,
		Kind: charge.AgreementCardOnFile, Lineage: lineage, AcceptedAt: now})
	return err
}

// RevokeReuse withdraws a card's one-click reuse.
func RevokeReuse(ctx context.Context, q *gen.Queries, merchantID, methodID uuid.UUID, now time.Time) error {
	_, err := q.RevokeCardOnFileMandates(ctx, gen.RevokeCardOnFileMandatesParams{MerchantID: merchantID, PaymentMethodID: methodID, Now: now.UTC()})
	return err
}

// EndForPaymentMethod ends every live mandate on a card; a zero now is the
// database's time.
func EndForPaymentMethod(ctx context.Context, q *gen.Queries, merchantID, methodID uuid.UUID, reason string, now time.Time) ([]gen.BillingMandate, error) {
	p := gen.EndPaymentMethodMandatesParams{MerchantID: merchantID, PaymentMethodID: methodID, Reason: reason}
	if !now.IsZero() {
		at := now.UTC()
		p.Now = &at
	}
	return q.EndPaymentMethodMandates(ctx, p)
}

// EndForSubscription ends a subscription's live recurring mandate, if any.
func EndForSubscription(ctx context.Context, q *gen.Queries, merchantID, subscriptionID uuid.UUID, reason string, now time.Time) error {
	m, err := q.GetLiveRecurringMandateForShare(ctx, gen.GetLiveRecurringMandateForShareParams{MerchantID: merchantID, SubscriptionID: subscriptionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = q.EndMandate(ctx, gen.EndMandateParams{MerchantID: merchantID, ID: m.ID, Reason: reason, Now: now.UTC()})
	return err
}

func optional(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// List is one page of a customer's mandates, newest first.
func List(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, page billing.PageRequest) (billing.ListPage[billing.Mandate], error) {
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[billing.Mandate]{}, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[billing.Mandate]{}, err
	}
	rows, err := q.ListCustomerMandatesPage(ctx, gen.ListCustomerMandatesPageParams{MerchantID: merchantID, CustomerID: customerID, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)})
	if err != nil {
		return billing.ListPage[billing.Mandate]{}, err
	}
	cut := pagination.Cut(rows, limit, func(m gen.BillingMandate) any { return pagination.TimeID{At: m.CreatedAt, ID: m.ID} })
	return pagination.Map(cut, View), nil
}

// ListByIDs reads a customer's named mandates, newest first; unknown ones are
// absent.
func ListByIDs(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, ids []uuid.UUID) (billing.ListPage[billing.Mandate], error) {
	rows, err := q.ListCustomerMandatesByIDs(ctx, gen.ListCustomerMandatesByIDsParams{MerchantID: merchantID, CustomerID: customerID, Ids: ids})
	if err != nil {
		return billing.ListPage[billing.Mandate]{}, err
	}
	return pagination.Map(billing.ListPage[gen.BillingMandate]{Items: rows}, View), nil
}

// View is a mandate as the API shows it.
func View(m gen.BillingMandate) billing.Mandate {
	out := billing.Mandate{
		ID: billing.MandateID(m.ID), CustomerID: billing.CustomerID(m.CustomerID), PSPID: billing.PSPID(m.PspID),
		Kind: billing.MandateKind(m.Kind), Currency: m.Currency, Status: billing.MandateStatus(m.Status), EndedAt: m.EndedAt,
		CardBrand: m.CardBrand, InitialTransactionID: m.InitialTransactionID, NetworkTransactionID: m.NetworkTransactionID,
		TransactionLinkID: m.TransactionLinkID, AcceptedAt: m.AcceptedAt, CreatedAt: m.CreatedAt,
	}
	if m.PaymentMethodID != nil {
		id := billing.PaymentMethodID(*m.PaymentMethodID)
		out.PaymentMethodID = &id
	}
	if m.SubscriptionID != nil {
		id := billing.SubscriptionID(*m.SubscriptionID)
		out.SubscriptionID = &id
	}
	if m.EndReason != nil {
		reason := billing.MandateEndReason(*m.EndReason)
		out.EndReason = &reason
	}
	if m.StoringAttemptID != nil {
		id := billing.PaymentAttemptID(*m.StoringAttemptID)
		out.StoringAttemptID = &id
	}
	return out
}
