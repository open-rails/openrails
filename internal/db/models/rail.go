package models

import "strings"

// (Removed) GrantSource: use EntitlementSourceType instead (admin, grace, one_off, subscription)

// Rail is a payment GATEWAY integration OpenRails codes against. There is one
// adapter per rail under internal/integrations/<rail>. A rail hosts 1..N
// credentialed PSPs (openrails.psps); e.g. "mobius"
// and "paykings" are PSP NAMES on rail "nmi", not rails themselves.
type Rail string

const (
	RailNMI    Rail = "nmi"    // Card payments via the NMI gateway (PSPs: mobius, paykings, …)
	RailCCBill Rail = "ccbill" // CCBill gateway (self-contained)
	RailSolana Rail = "solana" // Solana crypto payments (self-contained)
	RailStripe Rail = "stripe" // Stripe gateway (subscriptions + one-time)
	RailPayPal Rail = "paypal" // PayPal gateway (self-contained)
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

// Channel is an off-rail mechanism for RECORDING a payment that never flowed
// through a gateway integration — admin comps and manually-entered payments
// (cash, bank transfer, etc.). A channel is NOT a rail: it has no adapter, no
// credentials, and no PSP. Off-rail payments are recorded in the
// same source column as the rail (payments.rail), so a value there is either a
// Rail or a Channel; the two enums keep the senses distinct in Go.
type Channel string

const (
	ChannelAdmin  Channel = "admin"  // Admin-initiated payment (comp / manual entry by an admin)
	ChannelManual Channel = "manual" // Off-channel payment recorded by an admin (cash, bank transfer, …)
)

// IsOffRailChannel reports whether a value in a `rail` column is a Channel
// rather than a Rail. It is the one place that decides which rows may carry no
// PSP: or#893 requires provider provenance on every real rail, and a channel
// has no provider to name. Mirrors the DB CHECKs
// payments_psp_required_on_rail / invoice_payments_psp_required_on_rail.
func IsOffRailChannel(rail string) bool {
	switch Channel(strings.ToLower(strings.TrimSpace(rail))) {
	case ChannelAdmin, ChannelManual:
		return true
	}
	return false
}
