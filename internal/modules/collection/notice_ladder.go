package collection

import "time"

// or#870 bucket 2: the payment-method notification LADDER.
//
// Bucket 2 stops charging on purpose — retrying an expired card burns attempts
// against an instrument that cannot succeed, annoys the issuer and risks fraud
// scoring. But stopping removes the only clock the customer was ever on: no
// charge attempt means no failure event means no notification. One notice at
// the moment of the decline and then silence is exactly the failure mode the
// doctrine names ("silence through a whole dunning cycle followed by a sudden
// cancellation"), and it is worse here than in bucket 1, because bucket 1 at
// least keeps failing loudly.
//
// So the rungs are a table, anchored to the park moment. Each is a real
// customer-facing reminder that their access is still on and one card update
// ends the problem. This is the bucket that recovers revenue; an expired card
// is the single most recoverable failure in billing, and the recovery rate is
// a function of how many times you ask.
//
// The ladder cannot terminate anything. Running out of rungs closes the row
// and leaves the subscription exactly where it was — entitlements intact,
// stored payment method untouched. "Or you will lose the subscription" is copy,
// not a mechanism: what actually resolves a parked row is the customer fixing
// the card, or the provider-verification plane finding provider truth.

// paymentMethodNoticeOffsets are the rung moments as OFFSETS from the park.
//
// Rung 0 is the decline itself, sent inline by the failure flow — the ladder
// row is opened with it already counted. The later rungs are the ones a worker
// has to come back for.
//
// Anchoring every rung to parked_at (rather than to the previous send) means a
// late worker run never compounds its lateness into the following rungs, and a
// worker that has been down for a week sends the rungs that came due while it
// was down without stacking them into a burst.
var paymentMethodNoticeOffsets = []time.Duration{
	0,                   // the decline — "your card was declined, we stopped charging it"
	3 * 24 * time.Hour,  // the reminder — most customers act here or not at all
	10 * 24 * time.Hour, // the final notice
}

// PaymentMethodNoticeRungs is how many rungs the ladder has in total.
func PaymentMethodNoticeRungs() int { return len(paymentMethodNoticeOffsets) }

// NextPaymentMethodNoticeAt returns when the NEXT rung falls due, given how
// many rungs have already been sent (1 immediately after the park). ok=false
// means the ladder is spent — nothing further is sent, and nothing further
// happens to the subscription either.
func NextPaymentMethodNoticeAt(rungsSent int, parkedAt time.Time) (time.Time, bool) {
	if rungsSent < 1 || rungsSent >= len(paymentMethodNoticeOffsets) {
		return time.Time{}, false
	}
	return parkedAt.Add(paymentMethodNoticeOffsets[rungsSent]), true
}

// IsFinalPaymentMethodNotice reports whether the rung about to be sent — the
// (rungsSent+1)-th — is the last one. The copy differs: the final notice is the
// last time we ask, and saying so is the difference between a reminder and a
// nag.
func IsFinalPaymentMethodNotice(rungsSent int) bool {
	return rungsSent+1 >= len(paymentMethodNoticeOffsets)
}
