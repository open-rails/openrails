package spendgate

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/retention"
)

// SpendClaims are a request's spend windows as currency limits on their
// hashed anchors, each claiming cost, in policy order.
func SpendClaims(merchantID, customer uuid.UUID, currency string, policy Policy, req Request, cost int64, now time.Time) ([]limits.Claim, error) {
	windows := policy.EffectiveWindows(req)
	claims := make([]limits.Claim, 0, len(windows))
	for _, w := range windows {
		key, start, end, err := windowPeriod(merchantID, customer, currency, w, now)
		if err != nil {
			return nil, err
		}
		claims = append(claims, limits.Claim{Key: limits.SpendKeyPrefix + key, Unit: currency,
			Reset:  catalog.LimitReset{Kind: catalog.ResetFixed, WindowSeconds: int(w.Duration / time.Second)},
			Window: limits.Window{StartsAt: start, EndsAt: &end, Open: true}, Capacity: w.Limit, Cost: cost, Overrun: catalog.OverrunRequireEstimate})
	}
	return claims, nil
}

func fixedOffsetMs(prefix string, durMs int64) int64 {
	if durMs <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix))
	offset, _ := safecast.Convert[int64](h.Sum64() % uint64(durMs))
	return offset
}

func windowPeriod(mid, customer uuid.UUID, currency string, w resolvedWindow, now time.Time) (string, time.Time, time.Time, error) {
	if w.Duration < time.Millisecond || w.Duration > retention.MaxSpendWindow || w.Limit < 0 {
		return "", time.Time{}, time.Time{}, fmt.Errorf("invalid spend window")
	}
	key := w.identity(fmt.Sprintf("%s/%s/%s", mid, customer, currency))
	duration := w.Duration.Milliseconds()
	offset := fixedOffsetMs(key, duration)
	bucket := (now.UnixMilli() - offset) / duration
	start := time.UnixMilli(offset + bucket*duration).UTC()
	return key, start, start.Add(w.Duration), nil
}
