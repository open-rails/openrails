// Package nmimock is a fake NMI gateway for a host's end-to-end tests: it
// serves the NMI APIs OpenRails calls on a loopback listener, records what it
// was charged and declines on demand. It accepts any credentials and sends no
// webhooks. Only test code may import it.
//
// Point a sandbox engine at it, sharing one clock so a renewal is a clock step:
//
//	gateway := nmimock.New(nmimock.Options{Clock: clock.Now})
//	defer gateway.Close()
//	engine, err := openrails.New(ctx, openrails.Config{
//		ProviderSandbox: &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()},
//		Merchants: []billing.MerchantDeclaration{{MerchantDocument: billing.MerchantDocument{
//			Slug: "shop", DisplayName: "Shop", PSPs: billing.PSPs{
//				"nmi": openrails.NMIPSP{Environment: billing.EnvironmentSandbox, GatewayID: "test", SecurityKey: "test", WebhookSigningSecret: "test", TokenizationKey: "test"}.PSPConfig(),
//			},
//		}}},
//	}, openrails.Deps{Postgres: pool, Clock: clock})
//	id, err := engine.DeclaredMerchant("shop")
//	shop, err := engine.Merchant(id)
//
// Tokenize stands in for Collect.js; SetDecline(card.Last4, InsufficientFunds)
// makes that stored card's next charge decline, sending a renewal into dunning.
package nmimock

import (
	"time"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
)

// InsufficientFunds is NMI's decline code 202, a card the issuer refuses for
// lack of funds.
const InsufficientFunds = "202"

// Card is a card as a browser tokenizes it and a vault stores it.
type Card struct {
	Brand, Last4 string
	// Decline is the issuer's answer: "" approves, an NMI response code such
	// as InsufficientFunds declines its sales, and "vault" refuses storing
	// the card.
	Decline string
	// AVS and CVV are the verification letters every answer for this card
	// carries (avsresponse, cvvresponse); CVV "N" is a security-code mismatch.
	AVS, CVV string
	// BIN is the card's first six digits; empty derives one from Brand.
	BIN string
	// NetworkToken: the network token stands in for the card number, and the
	// Query API reports network_token_used on its charges.
	NetworkToken bool
	// Exp is the MMYY a card stored by number was given; empty reads as 1235.
	Exp string
	// VaultsDeclined: a declined sale that asked to save this card vaults it
	// anyway and answers its customer_vault_id.
	VaultsDeclined bool
}

// LedgerEntry is the money one approved sale moved, in cents, and its refunds.
type LedgerEntry struct {
	TransactionID, OrderID, Vault string
	Last4                         string
	Cents, RefundedCents          int64
}

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
	Amount   string
	Currency catalog.Currency
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
func (g *Mock) Tokenize(c Card) string { return g.m.Tokenize(nmimock.Card(c)) }

// SetDecline sets how the issuer answers for every stored card ending in
// last4: "" approves, a code such as InsufficientFunds declines.
func (g *Mock) SetDecline(last4, code string) { g.m.SetDecline(last4, code) }

// LastSale is the latest approved sale, or nil.
func (g *Mock) LastSale() *Sale { return sale(g.m.LastSale()) }

// LastDecline is the latest declined sale, or nil.
func (g *Mock) LastDecline() *Sale { return sale(g.m.LastDecline()) }

// Ledger is the money approved, unvoided sales moved, for one vault or ("")
// all.
func (g *Mock) Ledger(vault string) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range g.m.Ledger(vault) {
		out = append(out, LedgerEntry(e))
	}
	return out
}

// Charged is the net cents moved for one vault ("" all), optionally of one
// order ("" all).
func (g *Mock) Charged(vault, order string) int64 { return g.m.Charged(vault, order) }

// Unexpected lists requests the gateway does not model; a test asserts it
// empty.
func (g *Mock) Unexpected() []string { return g.m.Unexpected() }

// Plan, Vault (with its further Billing entries) and Schedule are an
// existing account's records: a legacy book a host imports.

// Plan is a Recurring Plan. Months, when set, is the cadence; otherwise Days
// (30 when zero).
type Plan struct {
	ID, Name, Amount string
	Days, Months     int
}

// Vault is a Customer Vault record: its priority-1 billing entry, then Extra.
type Vault struct {
	ID, BillingID string
	// Book is the merchant-defined field 20 the vault carries: the billing
	// book that created it.
	Book  string
	Card  Card
	Extra []Billing
	// NoBrand omits card_type from reads, as some live accounts do.
	NoBrand bool
	// Email and the names are the billing entry's; ShippingID names the
	// shipping entry (NMI generates one when empty).
	Email, FirstName, LastName, ShippingID string
	// Created is when the vault was stored.
	Created time.Time
}

// Billing is a further billing entry (card) in a vault.
type Billing struct {
	ID   string
	Card Card
}

// Schedule is an NMI recurring subscription. A deleted schedule stays
// readable as a tombstone (delayed_condition=inactive) and leaves lists.
type Schedule struct {
	ID, Vault, Plan, Amount string
	// Book is the merchant-defined field 20 the schedule carries. Created is
	// when it was made (start_date); zero means now.
	Book    string
	Created time.Time
	Order   string
	// Email is the schedule's billing email; empty reads the vault's.
	Email string
	// Days or Months is the cadence (Days 30 when both are zero).
	Days, Months int
	// CalendarAnchor is the original provider billing day, retained when a
	// shorter month clamps NextBilling. Zero keeps NextBilling's day.
	CalendarAnchor  time.Time
	NextBilling     time.Time
	Deleted, Paused bool
	// Custom is a custom-amount schedule: its plan shows an empty plan_name.
	Custom bool
}

// AddPlan stores a recurring plan the account already has.
func (g *Mock) AddPlan(p Plan) { g.m.AddPlan(nmimock.Plan(p)) }

// PutVault stores a vault as given (its ids, card, email and shipping id).
func (g *Mock) PutVault(v Vault) { g.m.PutVault(mockVault(v)) }

func mockVault(v Vault) nmimock.Vault {
	var extra []nmimock.Billing
	for _, b := range v.Extra {
		extra = append(extra, nmimock.Billing{ID: b.ID, Card: nmimock.Card(b.Card)})
	}
	return nmimock.Vault{ID: v.ID, BillingID: v.BillingID, Book: v.Book, Card: nmimock.Card(v.Card), Extra: extra, NoBrand: v.NoBrand,
		Email: v.Email, FirstName: v.FirstName, LastName: v.LastName, ShippingID: v.ShippingID, Created: v.Created}
}

// AddSchedule stores a recurring subscription the account bills and returns
// its id.
func (g *Mock) AddSchedule(s Schedule) string { return g.m.AddSchedule(nmimock.Schedule(s)) }

// AddScheduleSale records the approved charge NMI's recurring engine made
// for a schedule at a time.
func (g *Mock) AddScheduleSale(id string, at time.Time) *Sale {
	s := g.m.AddScheduleSale(id, at)
	return sale(&s)
}

// Mutations counts the writes the gateway received: a read-only caller
// leaves it zero.
func (g *Mock) Mutations() int { return len(g.m.Calls()) }

func sale(s *nmimock.Sale) *Sale {
	if s == nil {
		return nil
	}
	return &Sale{TransactionID: s.TransactionID, OrderID: s.OrderID, Vault: s.Vault, Last4: s.Card.Last4,
		Amount: s.Amount, Currency: catalog.Currency(s.Currency), Declined: s.Declined, At: s.At}
}
