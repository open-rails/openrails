package collection

// The certainty legs that may justify a TERMINAL collection outcome: a local
// cancel + entitlement revocation, and the queued cancel of the recurring
// schedule at the rail. A terminal outcome never touches the stored payment
// method; the card stays for the customer to update or remove.
//
// Never certainty: a date comparison (NMI rebills forever, so a lapsed
// next_billing_date is normal while dunning), a zero-length dunning window, or
// a missing row of our own (a sync bug looks like a dead card). Those park as
// `unknown` until a provider probe resolves them.
const (
	// CertaintyProviderConfirmedDead: the provider says the schedule is gone
	// (roster canceled/expired, or absent from a proven-exhaustive roster).
	CertaintyProviderConfirmedDead = "provider_confirmed_dead"
	// CertaintyNonRetryableDecline: a recorded decline whose rail code means
	// the account cannot be charged again. Retryable and unrecognized codes
	// never qualify.
	CertaintyNonRetryableDecline = "non_retryable_decline"
	// CertaintyDunningExhausted: real recorded dunning attempts reached the
	// policy max. Never elapsed grace, an old date, or an attempt skipped for
	// missing data.
	CertaintyDunningExhausted = "dunning_exhausted"
)
