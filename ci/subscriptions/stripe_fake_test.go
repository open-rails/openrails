//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"sync"

	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
)

// card is what a browser tokenizes. Decline is "" (approve), a Stripe
// decline_code / NMI response_code, or "auth" (issuer authentication).
type card = nmimock.Card

var (
	visa       = card{Brand: "visa", Last4: "4242"}
	mastercard = card{Brand: "mastercard", Last4: "4444"}
)

type obj = map[string]any

// providerCall is one journaled provider mutation.
type providerCall = stripemock.Call

// gate parks one matching provider request until released or its caller
// abandons it. commit decides whether an abandoned request still took
// effect at the provider (commit-then-loss) or never arrived.
type gate struct {
	match   func(*http.Request) bool
	arrived chan struct{}
	release chan struct{}
	commit  bool
	// served parks the response after the provider has acted.
	served bool
	once   sync.Once
}

func newGate(match func(*http.Request) bool, commit bool) *gate {
	return &gate{match: match, arrived: make(chan struct{}), release: make(chan struct{}), commit: commit}
}

// park is gate g as a mock interceptor.
func (g *gate) park(r *http.Request, serve func() *http.Response) (*http.Response, error) {
	var res *http.Response
	if g.served {
		res = serve()
	}
	g.once.Do(func() { close(g.arrived) })
	select {
	case <-g.release:
	case <-r.Context().Done():
		if g.commit && res == nil {
			serve()
		}
		return nil, r.Context().Err()
	}
	if res == nil {
		res = serve()
	}
	return res, nil
}

// stripeFake is the world's Stripe: stripemock plus conversions to this
// suite's shared provider types (card, gate, ledgerEntry).
type stripeFake struct{ *stripemock.Mock }

func newStripeFake() *stripeFake { return &stripeFake{stripemock.NewUnstarted(stripemock.Options{})} }

// hold parks matching requests on g.
func (f *stripeFake) hold(g *gate) *gate {
	f.Intercept(g.match, g.park)
	return g
}

func (f *stripeFake) unhold() { f.ClearIntercepts() }

// completeSetup is the browser confirming a SetupIntent with a new card.
func (f *stripeFake) completeSetup(setupID string, c card) {
	f.CompleteSetup(setupID, stripemock.Card{Brand: c.Brand, Last4: c.Last4, Decline: c.Decline})
}

// ledgerEntry is a provider's view of money one charge moved and what of it
// was refunded.
type ledgerEntry struct {
	ID, Charge, Method string
	Amount             int64
	Refunded           int64
}

// ledger is Stripe's succeeded charges for a customer ("" = all).
func (f *stripeFake) ledger(stripeCustomer string) []ledgerEntry {
	var out []ledgerEntry
	for _, e := range f.Ledger(stripeCustomer) {
		out = append(out, ledgerEntry{ID: e.PaymentIntent, Charge: e.Charge, Method: e.PaymentMethod, Amount: e.Amount, Refunded: e.Refunded})
	}
	return out
}
