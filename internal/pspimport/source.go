package pspimport

import (
	"context"
	"encoding/json"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/reconcile"
)

// staged is one provider object as a source read it.
type staged struct {
	kind            billing.PSPImportRecordKind
	ref             string
	customerRef     string
	subscriptionRef string
	occurredAt      time.Time
	facts           facts
	data            any
	// stated is the plan a subscription carries a copy of (NMI): what the
	// pull records when the provider no longer has the plan itself.
	stated *providerobjects.Object
	// merge fills a record other reads also fill: its data and facts replace
	// only their own keys (data is then a partial object).
	merge bool
}

// source reads one PSP. Implementations only read: list, report and get
// calls on a read-only client.
type source interface {
	// roster reads the PSP's customers and live subscriptions now.
	roster(ctx context.Context) ([]staged, error)
	// history reads the transactions in [since, until). The last window ends
	// at the pull: a provider that selects by last change reads through now.
	history(ctx context.Context, since, until time.Time, last bool) ([]staged, error)
}

// customerData is a provider customer's saved cards. Remote is the vault as
// a pull reads it: what a reconcile diffs a held one by.
type customerData struct {
	Cards  []cardData                      `json:"cards"`
	Remote []reconcile.RemotePaymentMethod `json:"remote,omitempty"`
}

type cardData struct {
	MethodRef string `json:"method_ref"`
	Primary   bool   `json:"primary,omitempty"`
	Brand     string `json:"brand,omitempty"`
	Last4     string `json:"last4,omitempty"`
	ExpMonth  int    `json:"exp_month,omitempty"`
	ExpYear   int    `json:"exp_year,omitempty"`
}

// subscriptionData is a provider subscription's terms and, once it ended, its
// end. Amount is in the currency's native units.
type subscriptionData struct {
	Plan     string `json:"plan,omitempty"`
	Amount   int64  `json:"amount,string"`
	Currency string `json:"currency"`
	// Decimal is a major-unit amount a provider states without a currency
	// (NMI); the subscription's price denominates it.
	Decimal        string     `json:"decimal,omitempty"`
	IntervalHours  int        `json:"interval_hours,omitempty"`
	IntervalMonths int        `json:"interval_months,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	NextBillingAt  *time.Time `json:"next_billing_at,omitempty"`
	// Ended is a subscription the provider no longer bills. EndedAt is when
	// its paid service ended; CanceledAt when someone canceled it; EndKind
	// why it ended (billing.CancelEvidence's kinds), "" for a cancel when
	// CanceledAt is known, else a provider's end.
	Ended      bool       `json:"ended,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	CanceledAt *time.Time `json:"canceled_at,omitempty"`
	EndKind    string     `json:"end_kind,omitempty"`
	// Stated are the recurring terms a provider stated at signup (CCBill),
	// which price a subscription that never rebilled.
	Stated *statedTerms `json:"stated,omitempty"`
	// MethodRef is the saved card the provider bills, when it names one.
	MethodRef string          `json:"method_ref,omitempty"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
	// Remote is the subscription as a pull reads it.
	Remote *reconcile.RemoteSubscription `json:"remote,omitempty"`
}

// statedTerms are recurring terms a provider states without a currency.
type statedTerms struct {
	Decimal       string `json:"decimal"`
	IntervalHours int    `json:"interval_hours"`
}

// transactionData is one charge, decline or refund. Amount is in the
// currency's native units.
type transactionData struct {
	// TransactionID is the provider's id; a refund's record ref also names
	// the refund.
	TransactionID         string    `json:"transaction_id"`
	OriginalTransactionID string    `json:"original_transaction_id,omitempty"`
	Type                  string    `json:"type"`
	Success               bool      `json:"success"`
	Amount                int64     `json:"amount,string"`
	Currency              string    `json:"currency"`
	OccurredAt            time.Time `json:"occurred_at"`
	// Decimal is a major-unit amount a provider reports without a currency;
	// the subscription's price denominates it.
	Decimal string `json:"decimal,omitempty"`
	// StripeInvoice is the complete invoice a Stripe charge paid.
	StripeInvoice *billing.DeclaredStripeInvoice `json:"stripe_invoice,omitempty"`
	// Remote is the transaction as a pull reads it.
	Remote *reconcile.RemoteTransaction `json:"remote,omitempty"`
}

// attribution places transactions on the subscription they paid when the
// provider does not name it: by the subscription's order id, else by a
// vault with one subscription that had started.
type attribution struct {
	byOrder map[string][]string
	byVault map[string][]vaultSub
}

type vaultSub struct {
	ref     string
	started time.Time
}

func newAttribution() *attribution {
	return &attribution{byOrder: map[string][]string{}, byVault: map[string][]vaultSub{}}
}

func (a *attribution) addSubscription(ref, customerRef string, f facts, started time.Time) {
	for _, order := range f[FactOrderID] {
		a.byOrder[order] = append(a.byOrder[order], ref)
	}
	if customerRef != "" {
		a.byVault[customerRef] = append(a.byVault[customerRef], vaultSub{ref: ref, started: started})
	}
}

// place fills a transaction's subscription_ref when exactly one subscription
// fits.
func (a *attribution) place(t *staged) {
	if t.subscriptionRef != "" {
		return
	}
	for _, order := range t.facts[FactOrderID] {
		if subs := a.byOrder[order]; len(subs) == 1 {
			t.subscriptionRef = subs[0]
			return
		}
	}
	if subs := a.byVault[t.customerRef]; len(subs) == 1 && !t.occurredAt.Before(subs[0].started.Truncate(24*time.Hour)) {
		t.subscriptionRef = subs[0].ref
	}
}
