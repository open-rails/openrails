package observability

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DBMetrics collects database connection pool and query metrics.
type DBMetrics struct {
	poolOpenConns  metric.Int64Gauge
	poolIdleConns  metric.Int64Gauge
	poolInUseConns metric.Int64Gauge
	poolWaitCount  metric.Int64Counter
	poolWaitDur    metric.Float64Counter
	poolMaxIdleClosed metric.Int64Counter
	poolMaxLifeClosed  metric.Int64Counter

	// Query-level metrics (via bun query hook)
	queryDuration  metric.Float64Histogram
	queryErrors    metric.Int64Counter
}

// NewDBMetrics creates DB metrics instruments.
func NewDBMetrics(meter metric.Meter) *DBMetrics {
	d := &DBMetrics{}

	var err error
	d.poolOpenConns, err = meter.Int64Gauge(
		"db_pool_open_connections",
		metric.WithDescription("Number of open connections to the database"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	d.poolIdleConns, err = meter.Int64Gauge(
		"db_pool_idle_connections",
		metric.WithDescription("Number of idle connections in the pool"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	d.poolInUseConns, err = meter.Int64Gauge(
		"db_pool_inuse_connections",
		metric.WithDescription("Number of connections currently in use"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	d.poolWaitCount, err = meter.Int64Counter(
		"db_pool_wait_count_total",
		metric.WithDescription("Total number of connections waited for"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	d.poolWaitDur, err = meter.Float64Counter(
		"db_pool_wait_duration_seconds",
		metric.WithDescription("Total time blocked waiting for a new connection"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return d
	}

	d.poolMaxIdleClosed, err = meter.Int64Counter(
		"db_pool_closed_max_idle_total",
		metric.WithDescription("Total number of connections closed due to idle timeout"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	d.poolMaxLifeClosed, err = meter.Int64Counter(
		"db_pool_closed_max_lifetime_total",
		metric.WithDescription("Total number of connections closed due to connection time"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return d
	}

	// Query metrics
	d.queryDuration, err = meter.Float64Histogram(
		"db_query_duration_seconds",
		metric.WithDescription("Database query duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0),
	)
	if err != nil {
		return d
	}

	d.queryErrors, err = meter.Int64Counter(
		"db_query_errors_total",
		metric.WithDescription("Total number of database query errors"),
		metric.WithUnit("1"),
	)

	return d
}

// CollectPoolStats extracts pool stats from an underlying sql.DB and records them.
// It works with bun.DB instances by extracting the underlying sql.DB.
func (d *DBMetrics) CollectPoolStats(ctx context.Context, bunDB bun.IDB, lastStats *sql.DBStats) {
	if d == nil || bunDB == nil {
		return
	}

	// Try to extract underlying sql.DB from bun.DB
	var sqldb *sql.DB
	if realDB, ok := bunDB.(*bun.DB); ok && realDB.DB != nil {
		sqldb = realDB.DB
	}
	if sqldb == nil {
		return
	}

	stats := sqldb.Stats()

	// Use deltas from lastStats to avoid duplicate counters
	openConns := int64(stats.OpenConnections)
	idleConns := int64(stats.Idle)
	inUseConns := int64(stats.InUse)
	waitCount := int64(stats.WaitCount)
	waitDur := float64(stats.WaitDuration.Nanoseconds()) / 1e9
	maxIdleClosed := int64(stats.MaxIdleClosed)
	maxLifeClosed := int64(stats.MaxLifetimeClosed)

	if d.poolOpenConns != nil {
		d.poolOpenConns.Record(ctx, openConns)
	}
	if d.poolIdleConns != nil {
		d.poolIdleConns.Record(ctx, idleConns)
	}
	if d.poolInUseConns != nil {
		d.poolInUseConns.Record(ctx, inUseConns)
	}

	// Counters use deltas to avoid duplicates
	if lastStats != nil {
		if d.poolWaitCount != nil {
			delta := waitCount - int64(lastStats.WaitCount)
			if delta > 0 {
				d.poolWaitCount.Add(ctx, delta)
			}
		}
		if d.poolWaitDur != nil {
			delta := waitDur - float64(lastStats.WaitDuration.Nanoseconds())/1e9
			if delta > 0 {
				d.poolWaitDur.Add(ctx, delta)
			}
		}
		if d.poolMaxIdleClosed != nil {
			delta := maxIdleClosed - int64(lastStats.MaxIdleClosed)
			if delta > 0 {
				d.poolMaxIdleClosed.Add(ctx, delta)
			}
		}
		if d.poolMaxLifeClosed != nil {
			delta := maxLifeClosed - int64(lastStats.MaxLifetimeClosed)
			if delta > 0 {
				d.poolMaxLifeClosed.Add(ctx, delta)
			}
		}
	} else {
		// First collection, record absolute values
		if d.poolWaitCount != nil && waitCount > 0 {
			d.poolWaitCount.Add(ctx, waitCount)
		}
		if d.poolWaitDur != nil && waitDur > 0 {
			d.poolWaitDur.Add(ctx, waitDur)
		}
		if d.poolMaxIdleClosed != nil && maxIdleClosed > 0 {
			d.poolMaxIdleClosed.Add(ctx, maxIdleClosed)
		}
		if d.poolMaxLifeClosed != nil && maxLifeClosed > 0 {
			d.poolMaxLifeClosed.Add(ctx, maxLifeClosed)
		}
	}

	// Store current stats for next delta calculation
	*lastStats = stats
}

// BunQueryHook implements bun.QueryHook for query-level metrics.
func (d *DBMetrics) BunQueryHook() bun.QueryHook {
	if d == nil {
		return nil
	}
	return &dbQueryHook{metrics: d}
}

type dbQueryHook struct {
	metrics *DBMetrics
}

func (h *dbQueryHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	return ctx
}

func (h *dbQueryHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if h.metrics == nil {
		return
	}

	// Use built-in Operation() method
	op := event.Operation()

	// Extract table name
	table := extractTableName(event.Query)

	attrs := []attribute.KeyValue{
		attribute.String("operation", op),
		attribute.String("table", table),
	}

	// Record duration
	duration := time.Since(event.StartTime).Seconds()
	if h.metrics.queryDuration != nil && duration > 0 {
		h.metrics.queryDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
	}

	// Record errors
	if event.Err != nil {
		errType := classifyError(event.Err)
		errorAttrs := append(attrs, attribute.String("error", errType))
		if h.metrics.queryErrors != nil {
			h.metrics.queryErrors.Add(ctx, 1, metric.WithAttributes(errorAttrs...))
		}
	}
}

func extractTableName(query string) string {
	// Very rough extraction - works for common cases
	parts := strings.Fields(strings.ToLower(query))
	for i, part := range parts {
		for _, keyword := range []string{"from", "into", "update", "join", "table", "truncate"} {
			if part == keyword && i+1 < len(parts) {
				// Next field is likely the table name
				table := parts[i+1]
				// Clean up quotes and schema prefix
				table = strings.Trim(table, "\"'")
				if idx := strings.Index(table, "."); idx >= 0 {
					table = table[idx+1:]
				}
				return table
			}
		}
	}
	return "unknown"
}

func classifyError(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "timeout") {
		return "timeout"
	}
	if strings.Contains(strings.ToLower(msg), "connection") {
		return "connection"
	}
	if strings.Contains(strings.ToLower(msg), "deadlock") {
		return "deadlock"
	}
	if strings.Contains(strings.ToLower(msg), "constraint") {
		return "constraint"
	}
	if strings.Contains(strings.ToLower(msg), "duplicate") {
		return "duplicate"
	}
	if strings.Contains(strings.ToLower(msg), "not found") {
		return "not_found"
	}
	return "other"
}

// ExtractSQLDB extracts the underlying sql.DB from a bun.IDB.
func ExtractSQLDB(db bun.IDB) (*sql.DB, bool) {
	if realDB, ok := db.(*bun.DB); ok {
		return realDB.DB, true
	}
	return nil, false
}
