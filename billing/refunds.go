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
// RevokeAccess also ends the entitlements and product access the payment
// granted, in the refund's transaction; for a subscription payment it ends the
// current access and cancels an engine-owned subscription so it does not
// renew. The provider's notification of an OpenRails refund never changes
// this decision.
type RefundPaymentParams struct {
	Amount         int64  `json:"amount,omitempty,string"`
	Full           bool   `json:"full,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RevokeAccess   bool   `json:"revoke_access,omitempty"`
	IdempotencyKey string `json:"-"`
}
