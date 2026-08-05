package admission

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// microsPerHour is the canonical rate unit on every or#897 surface: the declared
// cap, the host's prospective delta, and the measured rate all speak it, so no
// caller ever has to know the merchant's measurement window.
const secondsPerHour = int64(3600)

// AccrualRateMeter measures a payer's CURRENT accrual rate — micros per hour —
// from rated usage, for the or#897 accrual_rate_cap quota.
//
// The measurement is a LOOKBACK, not a history: it sums usage_events over the
// policy's window and scales to an hour, so the read is bounded by what the
// payer did in that window and never by how long it has been a customer. It is
// served by ix_usage_events_payer_time (merchant_id, customer_id, occurred_at).
//
// What it can and cannot see, stated plainly because a quota that silently
// under-measures is worse than none: it observes what has been REPORTED. A
// deployment admitted seconds ago has not accrued yet, so the host's own
// inventory is always ahead of this reading. That is why admission takes the
// host's prospective DELTA as an input rather than trying to infer it — the
// measured rate answers "what is already running", the delta answers "what am I
// about to add", and only the host knows the second.
type AccrualRateMeter struct {
	db  *db.DB
	now func() time.Time
}

func NewAccrualRateMeter(database *db.DB) *AccrualRateMeter {
	return &AccrualRateMeter{db: database}
}

// SetClock overrides the clock (tests). Nil-safe.
func (m *AccrualRateMeter) SetClock(now func() time.Time) {
	if m != nil {
		m.now = now
	}
}

func (m *AccrualRateMeter) clock() time.Time {
	if m == nil || m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

// MeasuredRatePerHour returns the payer's accrual rate in micros per hour,
// measured over window. A non-positive window is treated as one hour.
func (m *AccrualRateMeter) MeasuredRatePerHour(ctx context.Context, payer identity.CustomerID, currency string, window time.Duration) (int64, error) {
	if m == nil || m.db == nil {
		return 0, nil
	}
	if window <= 0 {
		window = time.Hour
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	since := m.clock().Add(-window)
	var total int64
	err = m.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var qerr error
		total, qerr = m.db.Gen(ctx).SumUsageAmountSince(ctx, gen.SumUsageAmountSinceParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			Currency:   currency,
			Since:      since,
		})
		return qerr
	})
	if err != nil {
		return 0, err
	}
	return RatePerHour(total, window), nil
}

// RatePerHour scales an amount accrued over window into micros per hour.
//
// Integer arithmetic on purpose (money never touches float), and the division is
// LAST so a sub-hour window does not round its own numerator away: $1 in a
// 60-second window is $60/hour, not $0/hour.
func RatePerHour(amountInWindow int64, window time.Duration) int64 {
	seconds := int64(window / time.Second)
	if seconds <= 0 {
		seconds = secondsPerHour
	}
	if amountInWindow <= 0 {
		return 0
	}
	return amountInWindow * secondsPerHour / seconds
}
