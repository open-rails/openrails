package admission

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

// Denial capture (#733): denials are counted in Redis hourly hashes on the hot
// path (one HINCRBY, fire-and-forget) and flushed to
// openrails.admission_denials_hourly by a periodic river job. The hot path
// NEVER writes Postgres per-request.

// DenialKeyPrefix namespaces the hourly denial hashes:
// or:mdeny:<merchant-uuid>:<unix-hour> -> { "<customer-uuid>|<reason>": count }.
const DenialKeyPrefix = "or:mdeny:"

// denialKeyTTL bounds counter lifetime if the flush job is down (safety valve;
// unflushed counters are lost after this, which beats unbounded Redis growth).
const denialKeyTTL = 30 * 24 * time.Hour

// DenialRecorder increments the hourly denial counters.
type DenialRecorder struct {
	rdb redis.Cmdable
}

// NewDenialRecorder builds a recorder over the shared Redis client.
func NewDenialRecorder(rdb redis.Cmdable) *DenialRecorder {
	return &DenialRecorder{rdb: rdb}
}

// Record counts one denial. Best-effort: a Redis error is logged, never
// surfaced — recording must not affect the admission decision.
func (r *DenialRecorder) Record(ctx context.Context, merchantID, customerID, reason string, at time.Time) {
	if r == nil || r.rdb == nil || reason == "" {
		return
	}
	key := DenialKey(merchantID, at)
	field := customerID + "|" + reason
	pipe := r.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, field, 1)
	pipe.Expire(ctx, key, denialKeyTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		log.WithContext(ctx).WithError(err).Warn("admission denial counter increment failed")
	}
}

// DenialKey is the hourly hash key for a merchant.
func DenialKey(merchantID string, at time.Time) string {
	return fmt.Sprintf("%s%s:%d", DenialKeyPrefix, merchantID, at.UTC().Truncate(time.Hour).Unix())
}

// ParseDenialKey inverts DenialKey. ok=false for foreign keys.
func ParseDenialKey(key string) (merchantID string, hour time.Time, ok bool) {
	rest, found := strings.CutPrefix(key, DenialKeyPrefix)
	if !found {
		return "", time.Time{}, false
	}
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 {
		return "", time.Time{}, false
	}
	sec, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return rest[:i], time.Unix(sec, 0).UTC(), true
}

// ParseDenialField inverts the "<customer>|<reason>" hash field.
func ParseDenialField(field string) (customerID, reason string, ok bool) {
	i := strings.IndexByte(field, '|')
	if i <= 0 || i == len(field)-1 {
		return "", "", false
	}
	return field[:i], field[i+1:], true
}
