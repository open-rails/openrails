package billing

// Refusals of actions on provider-owned (legacy NMI-billed)
// subscriptions. Each is a 409 and changes nothing locally or at the provider.
const (
	// CodeProviderCancelHeld: a staff cancel would stop the provider's billing
	// schedule, and destructive provider actions are not armed for this
	// merchant. Nothing changed; an operator finding records the request. A
	// customer's own cancel is never held.
	CodeProviderCancelHeld ErrorCode = "provider_cancel_held"
	// CodePaymentMethodSameVault: the provider bills a vault's primary card,
	// so another card of the same vault cannot become the subscription's
	// payment method. Save the card as a new payment method instead.
	CodePaymentMethodSameVault ErrorCode = "payment_method_same_vault"
	// CodePaymentDuplicateRefused: the provider refused the charge unprocessed
	// because an identical charge (same card and amount) was just made. Nothing
	// was charged; the same request succeeds once the provider's duplicate
	// window has passed.
	CodePaymentDuplicateRefused ErrorCode = "payment_duplicate_refused"
)
