package models

// (Removed) GrantSource: use EntitlementSourceType instead (admin, grace, one_off, subscription)

// Rail is a payment GATEWAY integration OpenRails codes against. There is one
// adapter per rail under internal/integrations/<rail>. A rail hosts 1..N
// credentialed PSPs (billing.psps); e.g. "mobius"
// and "paykings" are PSP NAMES on rail "nmi", not rails themselves.
type Rail string

const (
	RailNMI    Rail = "nmi"    // Card payments via the NMI gateway (PSPs: mobius, paykings, …)
	RailCCBill Rail = "ccbill" // CCBill gateway (self-contained)
	RailSolana Rail = "solana" // Solana crypto payments (self-contained)
	RailStripe Rail = "stripe" // Stripe gateway (subscriptions + one-time)
)

// EventSource is WHO sent an inbound provider event. Almost always a rail — but
// a CUSTODIAN sends its own events too (Basis Theory token/network-token
// lifecycle), and a custodian is not a gateway (or#879). Rail and Custodian
// namespaces are disjoint, so one string identifies either without ambiguity;
// the type exists so the two axes are never silently interchanged.
type EventSource string

// EventSource returns the rail as an inbound-event source.
func (r Rail) EventSource() EventSource { return EventSource(r) }

// EventSourceBasisTheory: the custodian, not the NMI rail it proxies into.
const EventSourceBasisTheory EventSource = EventSource(CustodianBasisTheory)

// Channel is how a payment's money arrived: through a PSP on a rail, or
// recorded by the merchant with no provider (cash, bank transfer). A manual
// payment has no rail and no PSP.
type Channel string

const (
	ChannelRail   Channel = "rail"
	ChannelManual Channel = "manual"
)
