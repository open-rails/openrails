package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

const (
	DefaultInvoiceCollectionThresholdAmount int64 = 50_000_000
	DefaultInvoiceMonthlyFloorAmount        int64 = 1_000_000

	InvoiceBoundaryCalendarMonth = "calendar_month"
	InvoiceBoundaryFixedInterval = "fixed_interval"
	InvoiceBoundaryAnniversary   = "anniversary"

	fixedInvoiceInterval = 30 * 24 * time.Hour
)

type InvoiceSettings struct {
	CollectionThresholdAmount int64
	MonthlyFloorAmount        int64
	BillingPeriodBoundary     string
}

type InvoiceThresholdOptions struct {
	CollectionThresholdAmount int64
	BillingPeriodBoundary     string
}

func (s *MoneyService) InvoiceSettings(ctx context.Context) (InvoiceSettings, error) {
	out := InvoiceSettings{
		CollectionThresholdAmount: DefaultInvoiceCollectionThresholdAmount,
		MonthlyFloorAmount:        DefaultInvoiceMonthlyFloorAmount,
		BillingPeriodBoundary:     InvoiceBoundaryFixedInterval,
	}
	if s == nil || s.db == nil {
		return out, fmt.Errorf("money service not initialized")
	}
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return out, err
	}
	if cfg.InvoiceCollectionThreshold != nil && *cfg.InvoiceCollectionThreshold >= 0 {
		out.CollectionThresholdAmount = *cfg.InvoiceCollectionThreshold
	}
	if cfg.InvoiceMonthlyFloor != nil && *cfg.InvoiceMonthlyFloor >= 0 {
		out.MonthlyFloorAmount = *cfg.InvoiceMonthlyFloor
	}
	if strings.TrimSpace(cfg.InvoiceBillingBoundary) != "" {
		boundary := NormalizeInvoiceBoundary(cfg.InvoiceBillingBoundary)
		if boundary == "" {
			return out, fmt.Errorf("invalid invoice billing_period_boundary %q", cfg.InvoiceBillingBoundary)
		}
		out.BillingPeriodBoundary = boundary
	}
	return out, nil
}

func NormalizeInvoiceBoundary(boundary string) string {
	switch strings.ToLower(strings.TrimSpace(boundary)) {
	case "", InvoiceBoundaryFixedInterval:
		return InvoiceBoundaryFixedInterval
	case InvoiceBoundaryCalendarMonth:
		return InvoiceBoundaryCalendarMonth
	case InvoiceBoundaryAnniversary:
		return InvoiceBoundaryAnniversary
	default:
		return ""
	}
}

func PreviousInvoicePeriod(now, anchor time.Time, boundary string) (time.Time, time.Time, error) {
	to, err := CurrentInvoicePeriodStart(now, anchor, boundary)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch NormalizeInvoiceBoundary(boundary) {
	case InvoiceBoundaryCalendarMonth:
		return addMonthsClamped(to, -1), to, nil
	case InvoiceBoundaryAnniversary:
		return previousAnniversaryStart(to, anchor), to, nil
	default:
		return to.Add(-fixedInvoiceInterval), to, nil
	}
}

func CurrentInvoicePeriodStart(now, anchor time.Time, boundary string) (time.Time, error) {
	now = now.UTC()
	switch NormalizeInvoiceBoundary(boundary) {
	case InvoiceBoundaryCalendarMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case InvoiceBoundaryAnniversary:
		return currentAnniversaryStart(now, anchor), nil
	case InvoiceBoundaryFixedInterval:
		sec := now.Unix()
		start := sec - sec%int64(fixedInvoiceInterval/time.Second)
		return time.Unix(start, 0).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("invalid invoice billing_period_boundary %q", boundary)
	}
}

func currentAnniversaryStart(now, anchor time.Time) time.Time {
	anchor = anchor.UTC()
	if anchor.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	start := anniversaryInMonth(now.Year(), now.Month(), anchor)
	if start.After(now) {
		start = addMonthsClamped(start, -1)
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
