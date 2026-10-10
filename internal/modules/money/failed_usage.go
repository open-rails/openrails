package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Failed usage is work that cost the platform and did not deliver. A customer's
// own failures are forgiven up to its grace windows and charged past them; a
// delegated invoker's are never charged and count toward its cutoff windows,
// which admission enforces. The windows are fixed, aligned to the Unix epoch,
// and counted in failed_usage_windows inside the usage event's transaction.

// FailedUsageWindow is one grace or cutoff window: at most Limit of failed
// usage per Duration, in the event's currency.
type FailedUsageWindow struct {
	Key      string
	Duration time.Duration
	Limit    int64
}

type failedUsagePeriod struct {
	window     FailedUsageWindow
	start, end time.Time
}

func failedUsagePeriods(windows []FailedUsageWindow, now time.Time) ([]failedUsagePeriod, error) {
	out := make([]failedUsagePeriod, 0, len(windows))
	for _, w := range windows {
		if w.Duration < time.Second || w.Limit < 0 || strings.TrimSpace(w.Key) == "" {
			return nil, fmt.Errorf("invalid failed-usage window %q", w.Key)
		}
		secs := int64(w.Duration / time.Second)
		start := time.Unix(now.Unix()/secs*secs, 0).UTC()
		out = append(out, failedUsagePeriod{window: w, start: start, end: start.Add(time.Duration(secs) * time.Second)})
	}
	return out, nil
}

// failedUsageKey names whose windows count: the customer's grace (invoker
// empty) or a delegated invoker's cutoff.
type failedUsageKey struct {
	merchant, customer uuid.UUID
	currency, invoker  string
}

func (k failedUsageKey) used(ctx context.Context, q *gen.Queries, p failedUsagePeriod) (int64, error) {
	return q.GetFailedUsageWindowAmount(ctx, gen.GetFailedUsageWindowAmountParams{
		MerchantID: k.merchant, CustomerID: k.customer, Currency: k.currency, Invoker: k.invoker,
		WindowKey: p.window.Key, WindowStart: p.start,
	})
}

// graceLeft is what the strictest window still forgives.
func (k failedUsageKey) graceLeft(ctx context.Context, q *gen.Queries, periods []failedUsagePeriod) (int64, error) {
	var left int64
	for i, p := range periods {
		used, err := k.used(ctx, q, p)
		if err != nil {
			return 0, err
		}
		if remaining := max(p.window.Limit-used, 0); i == 0 || remaining < left {
			left = remaining
		}
	}
	return left, nil
}

// count adds amount to every window and drops windows that ended.
func (k failedUsageKey) count(ctx context.Context, q *gen.Queries, periods []failedUsagePeriod, amount int64, now time.Time) error {
	if amount <= 0 {
		return nil
	}
	for _, p := range periods {
		if err := q.AddFailedUsageWindow(ctx, gen.AddFailedUsageWindowParams{
			MerchantID: k.merchant, CustomerID: k.customer, Currency: k.currency, Invoker: k.invoker,
			WindowKey: p.window.Key, WindowStart: p.start, WindowEnd: p.end, Amount: amount,
		}); err != nil {
			return err
		}
	}
	_, err := q.PruneFailedUsageWindows(ctx, gen.PruneFailedUsageWindowsParams{MerchantID: k.merchant, Now: now})
	return err
}

// FailedUsageCutoffReached reports whether a delegated invoker's failed usage
// has reached any of its cutoff windows, and which.
func (s *MoneyService) FailedUsageCutoffReached(ctx context.Context, customer identity.CustomerID, invoker, currency string, windows []FailedUsageWindow) (bool, string, error) {
	if s == nil || s.db == nil {
		return false, "", fmt.Errorf("money service not initialized")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return false, "", err
	}
	periods, err := failedUsagePeriods(windows, s.now())
	if err != nil {
		return false, "", err
	}
	key := failedUsageKey{merchant: merchantID.UUID(), customer: customer.UUID(), currency: normalizeCurrency(currency), invoker: invoker}
	var reached string
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		for _, p := range periods {
			if p.window.Limit <= 0 {
				continue
			}
			used, err := key.used(ctx, q, p)
			if err != nil {
				return err
			}
			if used >= p.window.Limit {
				reached = p.window.Key
				return nil
			}
		}
		return nil
	})
	return reached != "", reached, err
}
