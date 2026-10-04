package db

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/retention"
)

// partitionState remembers, per process, the month whose writable partitions
// are known to exist.
type partitionState struct {
	month atomic.Int64 // unix time of that month's start; 0 = not yet ensured
	mu    sync.Mutex
	retry time.Time // after a failure, the earliest wall-clock time to try again
}

// EnsurePartitions makes sure a row dated now has its monthly partition, and
// the months ahead theirs. A write path calls it before its transaction, so
// rows never depend on the cleanup job having run: the cost is one comparison,
// and one catalog round trip the first time a process sees a month.
//
// A failure is logged, not returned: the months ahead normally exist already,
// and a write whose partition is truly missing fails on its own insert.
func (d *DB) EnsurePartitions(ctx context.Context, now time.Time) {
	// A tx-scoped wrapper runs inside the caller's transaction, whose entry
	// point already ensured; DDL never runs there.
	if d == nil || d.pool == nil || d.partitions == nil {
		return
	}
	month := retention.MonthStart(now).Unix()
	if d.partitions.month.Load() == month {
		return
	}
	d.partitions.mu.Lock()
	defer d.partitions.mu.Unlock()
	if d.partitions.month.Load() == month || time.Now().Before(d.partitions.retry) {
		return
	}
	if _, err := retention.EnsurePartitions(ctx, d.GenDirectory(), now); err != nil {
		d.partitions.retry = time.Now().Add(time.Minute)
		logrus.WithError(err).Error("ensure monthly partitions failed; retrying in a minute")
		return
	}
	d.partitions.month.Store(month)
}
