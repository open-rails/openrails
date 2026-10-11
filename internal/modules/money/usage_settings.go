package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

const (
	DefaultCollectionThresholdAmount int64 = 50_000_000
	DefaultMonthlyFloorAmount        int64 = 1_000_000

	BillingBoundaryCalendarMonth = merchantconfig.BillingBoundaryCalendarMonth
	BillingBoundaryFixedInterval = merchantconfig.BillingBoundaryFixedInterval
	BillingBoundaryAnniversary   = merchantconfig.BillingBoundaryAnniversary

	fixedBillingInterval = 30 * 24 * time.Hour
)

// UsageSettings are the merchant's usage billing knobs: the period sweep's
// boundary, the threshold pass's trigger and the monthly collection floor.
type UsageSettings struct {
	CollectionThresholdAmount int64
	MonthlyFloorAmount        int64
	BillingPeriodBoundary     string
}

func (s *MoneyService) UsageSettings(ctx context.Context) (UsageSettings, error) {
	out := UsageSettings{
		CollectionThresholdAmount: DefaultCollectionThresholdAmount,
		MonthlyFloorAmount:        DefaultMonthlyFloorAmount,
		BillingPeriodBoundary:     BillingBoundaryFixedInterval,
	}
	if s == nil || s.db == nil {
		return out, fmt.Errorf("money service not initialized")
	}
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return out, err
	}
	if cfg.CollectionThreshold != nil && *cfg.CollectionThreshold >= 0 {
		out.CollectionThresholdAmount = *cfg.CollectionThreshold
	}
	if cfg.MonthlyFloor != nil && *cfg.MonthlyFloor >= 0 {
		out.MonthlyFloorAmount = *cfg.MonthlyFloor
	}
	if strings.TrimSpace(cfg.BillingPeriodBoundary) != "" {
		boundary := merchantconfig.NormalizeBillingBoundary(cfg.BillingPeriodBoundary)
		if boundary == "" {
			return out, fmt.Errorf("invalid billing_period_boundary %q", cfg.BillingPeriodBoundary)
		}
		out.BillingPeriodBoundary = boundary
	}
	return out, nil
}

func PreviousBillingPeriod(now, anchor time.Time, boundary string) (time.Time, time.Time, error) {
	to, err := CurrentBillingPeriodStart(now, anchor, boundary)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch merchantconfig.NormalizeBillingBoundary(boundary) {
	case BillingBoundaryCalendarMonth:
		return addMonthsClamped(to, -1), to, nil
	case BillingBoundaryAnniversary:
		return previousAnniversaryStart(to, anchor), to, nil
	default:
		return to.Add(-fixedBillingInterval), to, nil
	}
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

func previousAnniversaryStart(current, anchor time.Time) time.Time {
	current = current.UTC()
	anchor = anchor.UTC()
	if anchor.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	prevMonth := time.Date(current.Year(), current.Month(), 1, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC).AddDate(0, -1, 0)
	return anniversaryInMonth(prevMonth.Year(), prevMonth.Month(), anchor)
}

func anniversaryInMonth(year int, month time.Month, anchor time.Time) time.Time {
	day := anchor.Day()
	if last := lastDayOfMonth(year, month); day > last {
		day = last
	}
	return time.Date(year, month, day, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
}

func addMonthsClamped(t time.Time, months int) time.Time {
	t = t.UTC()
	first := time.Date(t.Year(), t.Month(), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	target := first.AddDate(0, months, 0)
	day := t.Day()
	if last := lastDayOfMonth(target.Year(), target.Month()); day > last {
		day = last
	}
	return time.Date(target.Year(), target.Month(), day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func lastDayOfMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
