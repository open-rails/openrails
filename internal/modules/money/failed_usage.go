package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
)

// Failed usage is work that cost the platform and did not deliver. A customer's
// own failures are forgiven up to its grace windows and charged past them; a
// delegated invoker's are never charged and count toward its cutoff windows,
// which admission enforces. The windows are fixed, aligned to the Unix epoch,
// and counted in the event store's limit usage (failed:<currency>:<window>,
// with :<invoker> for a cutoff).

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

// FailedUsageCutoffReached reports whether a delegated invoker's failed usage
// has reached any of its cutoff windows, and which.
func (s *MoneyService) FailedUsageCutoffReached(ctx context.Context, customer identity.CustomerID, invoker, currency string, windows []FailedUsageWindow) (bool, string, error) {
	if s == nil || s.db == nil {
		return false, "", fmt.Errorf("money service not initialized")
	}
	if _, err := merchant.Require(ctx); err != nil {
		return false, "", err
	}
	periods, err := failedUsagePeriods(windows, s.now())
	if err != nil {
		return false, "", err
	}
	store := s.db.EventStore()
	if store == nil {
		return false, "", eventstore.Required("a delegated invoker's cutoff")
	}
	cur := normalizeCurrency(currency)
	for _, p := range periods {
		if p.window.Limit <= 0 {
			continue
		}
		rows, err := store.LimitUnits(ctx, billing.CustomerID(customer.UUID()), failedLimitKey(cur, p.window.Key, true, invoker), p.start, p.start)
		if err != nil {
			return false, "", err
		}
		var used int64
		for _, r := range rows {
			used = addSat(used, r.Units)
		}
		if used >= p.window.Limit {
			return true, p.window.Key, nil
		}
	}
	return false, "", nil
}
