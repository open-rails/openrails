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
// hour, for the accrual_rate_cap quota: the per-minute spend of the policy's
// window (settled usage) plus the rates of its running charges. Admission
// takes the host's prospective delta for what is about to start.
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

// MeasuredRatePerHour returns the payer's accrual rate in micros per hour:
// its usage measured over window (a non-positive window is one hour), plus
// what its running charges accrue.
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
	var total, running int64
	err = m.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := m.db.Gen(ctx)
		var qerr error
		total, qerr = q.SumUsageMinutesSince(ctx, gen.SumUsageMinutesSinceParams{
			MerchantID: tid.UUID(),
			CustomerID: payer.UUID(),
			Currency:   currency,
			Since:      since,
		})
		if qerr != nil {
			return qerr
		}
		running, qerr = q.SumRunningChargeRates(ctx, gen.SumRunningChargeRatesParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: currency})
		return qerr
	})
	if err != nil {
		return 0, err
	}
	return RatePerHour(total, window) + running, nil
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
