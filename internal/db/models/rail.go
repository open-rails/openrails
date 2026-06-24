package models

// (Removed) GrantSource: use EntitlementSourceType instead (admin, grace, one_off, subscription)

// Rail represents payment rail types
type Rail string

const (
	RailMobius Rail = "mobius" // Card payments via NMI gateway
	RailCCBill Rail = "ccbill" // CCBill rail (self-contained)
	RailSolana Rail = "solana" // Solana crypto payments (self-contained)
	RailStripe Rail = "stripe" // Stripe rail (subscriptions + one-time)
	RailPayPal Rail = "paypal" // PayPal rail (self-contained)
	RailAdmin  Rail = "admin"  // Admin-initiated payments (comps, manual payments via PayPal/cash/etc.)
	RailManual Rail = "manual" // Off-channel/manual payments recorded by admins (cash, bank transfer, etc.)
)
