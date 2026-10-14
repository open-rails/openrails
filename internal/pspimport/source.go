package pspimport

import (
	"context"
	"encoding/json"
	"time"

	"github.com/open-rails/openrails/billing"
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
}

// source reads one PSP. Implementations only read: list, report and get
// calls on a read-only client.
type source interface {
	// roster reads the PSP's customers and live subscriptions now.
	roster(ctx context.Context) ([]staged, error)
	// history reads the transactions in [since, until).
	history(ctx context.Context, since, until time.Time) ([]staged, error)
}

// customerData is a provider customer's saved cards.
type customerData struct {
	Cards []cardData `json:"cards"`
}

type cardData struct {
	MethodRef string `json:"method_ref"`
	Primary   bool   `json:"primary,omitempty"`
	Brand     string `json:"brand,omitempty"`
	Last4     string `json:"last4,omitempty"`
	ExpMonth  int    `json:"exp_month,omitempty"`
	ExpYear   int    `json:"exp_year,omitempty"`
}

// subscriptionData is a live provider subscription's terms. Amount is in the
// currency's native units.
type subscriptionData struct {
	Plan           string     `json:"plan,omitempty"`
	Amount         int64      `json:"amount,string"`
	Currency       string     `json:"currency"`
	IntervalHours  int        `json:"interval_hours,omitempty"`
	IntervalMonths int        `json:"interval_months,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	NextBillingAt  *time.Time `json:"next_billing_at,omitempty"`
	Paused         bool       `json:"paused,omitempty"`
	// Ended is a subscription the provider no longer bills: EndedAt is when,
	// UserCanceled whether the subscriber ended it.
	Ended        bool       `json:"ended,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	UserCanceled bool       `json:"user_canceled,omitempty"`
	// MethodRef is the saved card the provider bills, when it names one.
	MethodRef string          `json:"method_ref,omitempty"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
}

// transactionData is one charge, decline or refund. Amount is in the
// currency's native units.
type transactionData struct {
	// TransactionID is the provider's id; a refund's record ref also names
	// the refund.
	TransactionID string    `json:"transaction_id"`
	Type          string    `json:"type"`
	Success       bool      `json:"success"`
	Amount        int64     `json:"amount,string"`
	Currency      string    `json:"currency"`
	OccurredAt    time.Time `json:"occurred_at"`
	// StripeInvoice is the complete invoice a Stripe charge paid.
	StripeInvoice *billing.DeclaredStripeInvoice `json:"stripe_invoice,omitempty"`
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
