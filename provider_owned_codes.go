package openrails

// Refusals for member actions on provider-owned (legacy NMI-billed)
// subscriptions. Each is a 409 and changes nothing locally or at the provider.
const (
	// CodeProviderCancelHeld: cancelling would delete the provider's billing
	// schedule, and destructive provider actions are not armed for this
	// merchant. Nothing changed; an operator finding records the request.
	CodeProviderCancelHeld = "provider_cancel_held"
	// CodeTierChangeRequiresEngineBilling: the provider bills this
	// subscription on its own schedule. Take it over to OpenRails billing
	// (engine takeover) and change tier on the engine-owned membership.
	CodeTierChangeRequiresEngineBilling = "tier_change_requires_engine_billing"
	// CodePaymentMethodSameVault: the provider bills a vault's primary card,
	// so another card of the same vault cannot become the subscription's
	// payment method. Save the card as a new payment method instead.
	CodePaymentMethodSameVault = "payment_method_same_vault"
)
