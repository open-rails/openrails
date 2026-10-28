package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

const (
	BillingBoundaryCalendarMonth = merchantconfig.BillingBoundaryCalendarMonth
	BillingBoundaryFixedInterval = merchantconfig.BillingBoundaryFixedInterval
	BillingBoundaryAnniversary   = merchantconfig.BillingBoundaryAnniversary

	fixedBillingInterval = 30 * 24 * time.Hour
)

// billingPeriodBoundary is the merchant's billing_period_boundary, fixed
// interval when it declares none.
func (s *MoneyService) billingPeriodBoundary(ctx context.Context) (string, error) {
	if s == nil || s.db == nil {
		return BillingBoundaryFixedInterval, fmt.Errorf("money service not initialized")
	}
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return BillingBoundaryFixedInterval, err
	}
	if strings.TrimSpace(cfg.BillingPeriodBoundary) == "" {
		return BillingBoundaryFixedInterval, nil
	}
	boundary := merchantconfig.NormalizeBillingBoundary(cfg.BillingPeriodBoundary)
	if boundary == "" {
		return BillingBoundaryFixedInterval, fmt.Errorf("invalid billing_period_boundary %q", cfg.BillingPeriodBoundary)
	}
	return boundary, nil
}

func CurrentBillingPeriodStart(now, anchor time.Time, boundary string) (time.Time, error) {
	now = now.UTC()
	switch merchantconfig.NormalizeBillingBoundary(boundary) {
	case BillingBoundaryCalendarMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case BillingBoundaryAnniversary:
		return currentAnniversaryStart(now, anchor), nil
	case BillingBoundaryFixedInterval:
		sec := now.Unix()
		start := sec - sec%int64(fixedBillingInterval/time.Second)
		return time.Unix(start, 0).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("invalid billing_period_boundary %q", boundary)
	}
}

func currentAnniversaryStart(now, anchor time.Time) time.Time {
	anchor = anchor.UTC()
	if anchor.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	now = now.UTC()
	start := anniversaryInMonth(now.Year(), now.Month(), anchor)
	if start.After(now) {
		prev := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		start = anniversaryInMonth(prev.Year(), prev.Month(), anchor)
	}
	return start
}

func anniversaryInMonth(year int, month time.Month, anchor time.Time) time.Time {
	day := anchor.Day()
	if last := lastDayOfMonth(year, month); day > last {
		day = last
	}
	return time.Date(year, month, day, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
}

func lastDayOfMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
