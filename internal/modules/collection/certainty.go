package collection

// The certainty legs that may justify a TERMINAL collection outcome — a local
// cancel + entitlement revocation, and the irreversible cancellation of the
// RECURRING SCHEDULE at the rail that it queues (#664/#821/#839/#840).
//
// or#870: what a terminal outcome does NOT do is touch the stored payment
// method. OpenRails never deletes a customer's instrument — not on expiry, not
// on a stolen card, not on cancellation. The rail-side subscription dies; the
// card stays, for the customer to update or remove themselves.
//
// A terminal outcome is only ever reached through one of these. Explicitly NOT
// certainty: a date comparison (NMI rebills forever — a lapsed
// next_billing_date is the NORMAL state of every dunning customer), a
// zero-length dunning window, or the absence of one of OUR OWN rows (a
// vault-sync bug is indistinguishable from a dead card). Those PARK as
// `unknown` and a targeted provider probe resolves them.
//
// These constants live here — the collection engine — because BOTH consumers
// need them: the reconcile decider (which depends on collection) and the
// subscription lifecycle's FailMembership chokepoint.
const (
	// CertaintyProviderConfirmedDead: the provider itself says the schedule is
	// gone (roster cancelled/expired, or absent from a PROVEN-exhaustive
	// roster). Mirroring provider truth, not our inference.
	CertaintyProviderConfirmedDead = "provider_confirmed_dead"
	// CertaintyNonRetryableDecline: a recorded decline whose rail code means
	// the billing authorization is withdrawn / the account cannot be charged
	// again. Retryable and unrecognized codes never qualify.
	CertaintyNonRetryableDecline = "non_retryable_decline"
	// CertaintyDunningExhausted: real recorded dunning ATTEMPTS reached the
	// policy max. Never "grace elapsed", never "the date is old", and never an
	// attempt we declined to make because our own data was missing.
	CertaintyDunningExhausted = "dunning_exhausted"
)
