// Package nmimock is a fake NMI gateway for a host's end-to-end tests: it
// serves the NMI APIs OpenRails calls on a loopback listener, records what it
// was charged and declines on demand. It accepts any credentials and sends no
// webhooks. Only test code may import it.
//
// Point a sandbox engine at it, sharing one clock so a renewal is a clock step:
//
//	gateway := nmimock.New(nmimock.Options{Clock: clock.Now})
//	defer gateway.Close()
//	client, err := openrails.New(ctx, openrails.Config{
//		TestMode:          openrails.Sandbox,
//		ProviderWriteMode: openrails.ProviderWritesFull,
//		ProviderSandbox:   &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()},
//		Merchant: openrails.MerchantDeclaration{Slug: "shop", DisplayName: "Shop", PSPs: map[string]openrails.PSPConfig{
//			"nmi": openrails.NMIPSP{AccountID: "test", SecurityKey: "test", WebhookSigningSecret: "test", TokenizationKey: "test"}.PSPConfig(),
//		}},
//	}, openrails.Deps{Postgres: pool, Clock: clock})
//
// Tokenize stands in for Collect.js; SetDecline(card.Last4, InsufficientFunds)
// makes that stored card's next charge decline, sending a renewal into dunning.
package nmimock

import (
	"time"

	"github.com/open-rails/openrails/internal/nmimock"
)

// InsufficientFunds is NMI's decline code 202, a card the issuer refuses for
// lack of funds.
const InsufficientFunds = "202"

// Card is a card as a browser tokenizes it and a vault stores it. Decline ""
// approves; an NMI response code such as InsufficientFunds declines its sales.
type Card = nmimock.Card

// LedgerEntry is the money one approved sale moved, in cents, and its refunds.
type LedgerEntry = nmimock.LedgerEntry

// Options configure a Mock.
type Options struct {
	// Clock is the gateway's only time source; nil is time.Now.
	Clock func() time.Time
}

// Mock is a running fake gateway. Its methods are safe for concurrent use.
type Mock struct{ m *nmimock.Mock }

// Sale is one sale the gateway processed, approved or declined.
type Sale struct {
	TransactionID, OrderID, Vault, Last4 string
	// Amount is NMI's wire decimal ("9.99").
	Amount, Currency string
	// Declined is the decline code; "" is approved.
	Declined string
	At       time.Time
}

// New starts a gateway on a loopback listener; Close stops it.
func New(opts Options) *Mock {
	return &Mock{nmimock.New(nmimock.Options{Clock: opts.Clock})}
}

// URL is the gateway root: Config.ProviderSandbox.NMIGatewayURL.
func (g *Mock) URL() string { return g.m.URL() }

// Close stops the listener.
func (g *Mock) Close() { g.m.Close() }

// Tokenize is Collect.js: a payment token for c.
func (g *Mock) Tokenize(c Card) string { return g.m.Tokenize(c) }

// SetDecline sets how the issuer answers for every stored card ending in
// last4: "" approves, a code such as InsufficientFunds declines.
func (g *Mock) SetDecline(last4, code string) { g.m.SetDecline(last4, code) }

// LastSale is the latest approved sale, or nil.
func (g *Mock) LastSale() *Sale { return sale(g.m.LastSale()) }

// LastDecline is the latest declined sale, or nil.
func (g *Mock) LastDecline() *Sale { return sale(g.m.LastDecline()) }

// Ledger is the money approved, unvoided sales moved, for one vault or ("")
// all.
func (g *Mock) Ledger(vault string) []LedgerEntry { return g.m.Ledger(vault) }

// Charged is the net cents moved for one vault ("" all), optionally of one
// order ("" all).
func (g *Mock) Charged(vault, order string) int64 { return g.m.Charged(vault, order) }

// Unexpected lists requests the gateway does not model; a test asserts it
// empty.
func (g *Mock) Unexpected() []string { return g.m.Unexpected() }

func sale(s *nmimock.Sale) *Sale {
	if s == nil {
		return nil
	}
	return &Sale{TransactionID: s.TransactionID, OrderID: s.OrderID, Vault: s.Vault, Last4: s.Card.Last4,
		Amount: s.Amount, Currency: s.Currency, Declined: s.Declined, At: s.At}
}
