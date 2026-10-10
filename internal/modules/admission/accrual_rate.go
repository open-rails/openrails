package admission

import (
	"context"
	"time"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Rates are micros per hour everywhere (declared cap, host's prospective delta,
// measured rate), so no caller needs the merchant's measurement window.
const secondsPerHour = int64(3600)

// AccrualRateMeter measures a customer's current accrual rate, in micros per
// hour, for the accrual_rate_cap quota. It sums usage_events over the policy's
// window (served by usage_events_customer_id_occurred_at_idx), so it sees only
// reported usage: admission takes the host's prospective delta for what is
// about to start.
type AccrualRateMeter struct {
	db  *db.DB
	now func() time.Time
}

func NewAccrualRateMeter(database *db.DB) *AccrualRateMeter {
	return &AccrualRateMeter{db: database}
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

// RatePerHour scales an amount accrued over window into micros per hour, in
// integer math with the division last so a sub-hour window keeps its numerator
// ($1 in 60s is $60/hour).
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
