package subscriptions

type PremiumEndReason string

const (
	PremiumEndReasonUserCancel PremiumEndReason = "user_cancel"
	PremiumEndReasonExpired    PremiumEndReason = "expired"
	PremiumEndReasonChargeback PremiumEndReason = "chargeback"
	PremiumEndReasonRefund     PremiumEndReason = "refund"
	PremiumEndReasonAdmin      PremiumEndReason = "admin"
	PremiumEndReasonRail       PremiumEndReason = "rail_cancel"
	// PremiumEndReasonAccessEnded: the converge pass saw the customer's last
	// entitlement window close with no transition-site email. Neutral copy, no
	// charge or dunning language.
	PremiumEndReasonAccessEnded PremiumEndReason = "access_ended"
	// PremiumEndReasonNonRecoverable (bucket 3): the issuer withdrew the
	// recurring mandate or the instrument is dead. The rail schedule was
	// canceled, the stored payment method untouched; the copy invites a
	// re-subscribe.
	PremiumEndReasonNonRecoverable PremiumEndReason = "non_recoverable"
	PremiumEndReasonUnknown        PremiumEndReason = "unknown"
)
