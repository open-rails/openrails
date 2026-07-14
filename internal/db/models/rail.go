package models

// (Removed) GrantSource: use EntitlementSourceType instead (admin, grace, one_off, subscription)

// Rail is a payment GATEWAY integration OpenRails codes against. There is one
// adapter per rail under internal/integrations/<rail>. A rail hosts 1..N
// credentialed provider accounts (openrails.psps); e.g. "mobius"
// and "paykings" are provider-account NAMES on rail "nmi", not rails themselves.
type Rail string

const (
	RailNMI    Rail = "nmi"    // Card payments via the NMI gateway (provider accounts: mobius, paykings, …)
	RailCCBill Rail = "ccbill" // CCBill gateway (self-contained)
	RailSolana Rail = "solana" // Solana crypto payments (self-contained)
	RailStripe Rail = "stripe" // Stripe gateway (subscriptions + one-time)
	// RailVaultedCard (#795): neutral card vault (Basis Theory) detokenizing
	// through a proxy into an NMI gateway account. account_id = the BT tenant
	// id (operator-declared); the destination NMI account is referenced from
	// the account's settings (gateway_account).
	RailVaultedCard Rail = "vaulted_card"
	RailPayPal      Rail = "paypal" // PayPal gateway (self-contained)
)

// Channel is an off-rail mechanism for RECORDING a payment that never flowed
// through a gateway integration — admin comps and manually-entered payments
// (cash, bank transfer, etc.). A channel is NOT a rail: it has no adapter, no
// credentials, and no provider account. Off-rail payments are recorded in the
// same source column as the rail (payments.rail), so a value there is either a
// Rail or a Channel; the two enums keep the senses distinct in Go.
type Channel string

const (
	ChannelAdmin  Channel = "admin"  // Admin-initiated payment (comp / manual entry by an admin)
	ChannelManual Channel = "manual" // Off-channel payment recorded by an admin (cash, bank transfer, …)
)
