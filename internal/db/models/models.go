package models

import (
	"github.com/uptrace/bun"
)

// (Removed) GrantSource: use EntitlementSourceType instead (admin, grace, one_off, subscription)

// Processor represents payment processor types
type Processor string

const (
	ProcessorMobius Processor = "mobius" // Card payments via NMI gateway
	ProcessorCCBill Processor = "ccbill" // CCBill processor (self-contained)
	ProcessorSolana Processor = "solana" // Solana crypto payments (self-contained)
	ProcessorStripe Processor = "stripe" // Stripe processor (subscriptions + one-time)
	ProcessorPayPal Processor = "paypal" // PayPal processor (self-contained)
	ProcessorAdmin  Processor = "admin"  // Admin-initiated payments (comps, manual payments via PayPal/cash/etc.)
	ProcessorManual Processor = "manual" // Off-channel/manual payments recorded by admins (cash, bank transfer, etc.)
)

var ModelRegistry = []any{
	(*Product)(nil),
	(*Price)(nil),
	(*CheckoutSession)(nil),
	(*Payment)(nil),
	(*Subscription)(nil),
	(*PaymentMethod)(nil),
	(*NotificationQueue)(nil),
	(*Entitlement)(nil),
	(*AdminGrant)(nil),
	(*CreditType)(nil),
	(*UserCreditBalance)(nil),
	(*CreditTransaction)(nil),
	(*CreditBlock)(nil),
	(*ProcessorCustomer)(nil),
}

func RegisterModels(db *bun.DB) {
	db.RegisterModel(ModelRegistry...)
}
