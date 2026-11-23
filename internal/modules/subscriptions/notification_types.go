package subscriptions

// PremiumEndReason is why a subscription ended: the reason of its
// premium_ended fact, and of the subscription_canceled fact it becomes while
// paid access remains.
type PremiumEndReason string

const (
	// PremiumEndReasonUserCancel: the customer canceled.
	PremiumEndReasonUserCancel PremiumEndReason = "user_cancel"
	// PremiumEndReasonExpired: payments failed. Dunning spent its schedule,
	// the wait for a new card ran out, or the provider stopped after failed
	// rebills.
	PremiumEndReasonExpired    PremiumEndReason = "expired"
	PremiumEndReasonChargeback PremiumEndReason = "chargeback"
	// PremiumEndReasonRefund: a refund that revoked access ended it.
	PremiumEndReasonRefund PremiumEndReason = "refund"
	// PremiumEndReasonAdmin: the merchant's staff canceled it in OpenRails.
	PremiumEndReasonAdmin PremiumEndReason = "admin"
	// PremiumEndReasonRail: the provider or a system action ended it (a
	// provider-side cancel, a void, a tier change).
	PremiumEndReasonRail PremiumEndReason = "rail_cancel"
	// PremiumEndReasonAccessEnded: access reached its end with no cause
	// recorded: the converge pass saw the customer's last entitlement window
	// close with no transition-site notice, or a provider expired the term.
	// Neutral copy, no charge or dunning language.
	PremiumEndReasonAccessEnded PremiumEndReason = "access_ended"
	// PremiumEndReasonNonRecoverable (bucket 3): the issuer withdrew the
	// recurring mandate or the instrument is dead. The rail schedule was
	// canceled, the stored payment method untouched; the copy invites a
	// re-subscribe.
	PremiumEndReasonNonRecoverable PremiumEndReason = "non_recoverable"
	PremiumEndReasonUnknown        PremiumEndReason = "unknown"
)
