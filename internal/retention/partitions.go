package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// PartitionQueries is the DDL partition maintenance runs: both statements work
// from the calendar and the catalog, never from rows.
type PartitionQueries interface {
	EnsureMonthPartitions(ctx context.Context, arg gen.EnsureMonthPartitionsParams) (int32, error)
	DropMonthPartitions(ctx context.Context, arg gen.DropMonthPartitionsParams) (int32, error)
}

// EnsurePartitions creates the missing partitions rows can be written into at
// now, and the months ahead. It runs at migration and on every cleanup pass,
// so a database that sat idle past its last partition is whole again before
// the first write.
func EnsurePartitions(ctx context.Context, q PartitionQueries, now time.Time) (created int, err error) {
	return ensure(ctx, q, now, MonthlyPartitions.Range)
}

// EnsureRetainedPartitions creates every missing partition of the retained
// window: a restore brings back rows older than anything written here.
func EnsureRetainedPartitions(ctx context.Context, q PartitionQueries, now time.Time) (created int, err error) {
	return ensure(ctx, q, now, MonthlyPartitions.RetainedRange)
}

func ensure(ctx context.Context, q PartitionQueries, now time.Time, span func(MonthlyPartitions, time.Time) (time.Time, time.Time)) (created int, err error) {
	for _, p := range Partitioned {
		from, through := span(p, now)
		n, perr := q.EnsureMonthPartitions(ctx, gen.EnsureMonthPartitionsParams{TableName: p.Table, FromAt: from, ThroughAt: through})
		if perr != nil {
			err = errors.Join(err, fmt.Errorf("ensure %s partitions: %w", p.Table, perr))
			continue
		}
		created += int(n)
	}
	return created, err
}

// DropExpiredPartitions drops every partition whose whole month is past its
// table's retention at now.
func DropExpiredPartitions(ctx context.Context, q PartitionQueries, now time.Time) (dropped int, err error) {
	for _, p := range Partitioned {
		n, perr := q.DropMonthPartitions(ctx, gen.DropMonthPartitionsParams{TableName: p.Table, Before: p.DropBefore(now)})
		if perr != nil {
			err = errors.Join(err, fmt.Errorf("drop %s partitions: %w", p.Table, perr))
			continue
		}
		dropped += int(n)
	}
	return dropped, err
}
