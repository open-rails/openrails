package credits

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// LowBalanceEvent is the reusable low-balance SIGNAL emitted when an owner's
// available balance for a credit type drops below the type's configured
// threshold (issue #240). It completes the deferred #116 low-balance alert and
// is intentionally a small, processor-agnostic value object so downstream
// features can consume the SAME signal without re-deriving it:
//
//   - #239 (auto-top-up) listens for this to trigger a top-up charge;
//   - #241 (arrears) listens for this to flag an account heading negative.
//
// The signal is owner/tenant-scoped (issue #221/#223): OwnerID is the billing
// owner and TenantID the billing namespace. UserID is carried for ACTOR
// attribution only.
type LowBalanceEvent struct {
	TenantID       uuid.UUID
	OwnerID        uuid.UUID
	UserID         string
	CreditTypeID   uuid.UUID
	CreditTypeName string
	// Available is balance - held_balance at detection time.
	Available int64
	// Threshold is the credit type's low_balance_threshold that was crossed.
	Threshold int64
	// DetectedAt is when the alert job observed the under-threshold balance.
	DetectedAt time.Time
}

// LowBalanceSink consumes LowBalanceEvents. The low-balance alert job emits to a
// sink rather than to a concrete notifier so the SAME detection loop can fan the
// signal out to several consumers later (#239 auto-top-up, #241 arrears) by
// composing sinks — without the job knowing about any of them.
//
// Handle should be best-effort: returning an error is logged by the job but does
// NOT prevent last_low_balance_alert_at from being stamped, so a transient
// notification failure cannot cause the same owner to be re-alerted forever.
type LowBalanceSink interface {
	Handle(ctx context.Context, ev LowBalanceEvent) error
}

// LowBalanceSinkFunc adapts a plain function to LowBalanceSink.
type LowBalanceSinkFunc func(ctx context.Context, ev LowBalanceEvent) error

func (f LowBalanceSinkFunc) Handle(ctx context.Context, ev LowBalanceEvent) error {
	return f(ctx, ev)
}

// MultiLowBalanceSink fans a LowBalanceEvent out to several sinks in order. It is
// the composition point for #239/#241: register additional sinks alongside the
// notification sink without touching the alert job. Errors are collected; the
// first non-nil error is returned after every sink has been given the event.
type MultiLowBalanceSink []LowBalanceSink

func (m MultiLowBalanceSink) Handle(ctx context.Context, ev LowBalanceEvent) error {
	var firstErr error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Handle(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
