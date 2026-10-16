package billing

var (
	// ErrRefundRailUnavailable: the payment's rail cannot currently accept a
	// refund (account not armed or not configured). Nothing was reserved.
	ErrRefundRailUnavailable error = newCodedError("refund_rail_unavailable", ErrConflict)
	// ErrRefundUnsupported: the payment's rail has no automatic refund (CCBill,
	// off-rail channels). Refund it at the provider and record it there.
	ErrRefundUnsupported error = newCodedError("refund_unsupported", ErrInvalid)
)

// RefundPaymentParams refunds one charge. Exactly one of Amount (native units
// at the payment currency's scale) or Full is required. IdempotencyKey is
// mandatory: a retry with the same key returns the same refund and never
// refunds twice; the same key with different terms is ErrIdempotencyKeyReused.
// RevokeAccess removes only access funded by this payment, in the refund's
// transaction. Subscription renewal and newer paid periods are preserved.
// If seat or plan changes make the funding ambiguous, the refund completes
// and a merchant finding requests review while access stays intact.
// Provider notifications of an OpenRails refund preserve this decision.
type RefundPaymentParams struct {
	Amount         int64  `json:"amount,omitempty,string"`
	Full           bool   `json:"full,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RevokeAccess   bool   `json:"revoke_access,omitempty"`
	IdempotencyKey string `json:"-"`
}
