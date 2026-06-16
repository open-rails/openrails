package observability

import (
	"context"
	"database/sql"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
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
		log.WithError(err).WithField("instrument", "db_pool_open_connections").Error("failed to create OTel gauge")
	}

	d.poolIdleConns, err = meter.Int64Gauge(
		"db_pool_idle_connections",
		metric.WithDescription("Number of idle connections in the pool"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_idle_connections").Error("failed to create OTel gauge")
	}

	d.poolInUseConns, err = meter.Int64Gauge(
		"db_pool_inuse_connections",
		metric.WithDescription("Number of connections currently in use"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_inuse_connections").Error("failed to create OTel gauge")
	}

	d.poolWaitCount, err = meter.Int64Counter(
		"db_pool_wait_count_total",
		metric.WithDescription("Total number of connections waited for"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_wait_count_total").Error("failed to create OTel counter")
	}

	d.poolWaitDur, err = meter.Float64Counter(
		"db_pool_wait_duration_seconds",
		metric.WithDescription("Total time blocked waiting for a new connection"),
		metric.WithUnit("s"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_wait_duration_seconds").Error("failed to create OTel counter")
	}

	d.poolMaxIdleClosed, err = meter.Int64Counter(
		"db_pool_closed_max_idle_total",
		metric.WithDescription("Total number of connections closed due to idle timeout"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_closed_max_idle_total").Error("failed to create OTel counter")
	}

	d.poolMaxLifeClosed, err = meter.Int64Counter(
		"db_pool_closed_max_lifetime_total",
		metric.WithDescription("Total number of connections closed due to connection time"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_pool_closed_max_lifetime_total").Error("failed to create OTel counter")
	}

	// Query metrics
	d.queryDuration, err = meter.Float64Histogram(
		"db_query_duration_seconds",
		metric.WithDescription("Database query duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_query_duration_seconds").Error("failed to create OTel histogram")
	}

	d.queryErrors, err = meter.Int64Counter(
		"db_query_errors_total",
		metric.WithDescription("Total number of database query errors"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "db_query_errors_total").Error("failed to create OTel counter")
	}

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

	// Use built-in Operation() method for the SQL operation type
	op := event.Operation()

	// Extract table name from bun's model information (reliable) rather than SQL parsing.
	// This avoids breaking on CTEs, subqueries, and multi-table joins.
	table := extractTableNameFromEvent(event)

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

// extractTableNameFromEvent extracts the table name from a bun QueryEvent using
// the model information rather than SQL parsing. This is reliable for CTEs,
// subqueries, and multi-table joins where string parsing would fail.
func extractTableNameFromEvent(event *bun.QueryEvent) string {
	// Try to get the table name from the model first (most reliable).
	if event.Model != nil {
		if tableName, ok := tableNameFromModel(event.Model); ok {
			return tableName
		}
	}
	// Fallback: try to extract from the SQL query for ad-hoc queries without a model.
	return extractTableNameFromQuery(event.Query)
}

// tableNameFromModel attempts to extract a table name from a bun model value.
func tableNameFromModel(model bun.Model) (string, bool) {
	// bun.TableModel exposes the underlying schema.Table which has the name.
	var tm bun.TableModel
	var ok bool
	if tm, ok = model.(bun.TableModel); !ok {
		return "", false
	}
	table := tm.Table()
	if table == nil {
		return "", false
	}
	name := table.Name
	if name == "" {
		return "", false
	}
	return name, true
}

// extractTableNameFromQuery extracts a table name from SQL as a last resort.
// Uses regex-like positional matching that handles CTEs and subqueries better
// than naive field splitting.
func extractTableNameFromQuery(query string) string {
	lower := strings.ToLower(query)

	// Skip CTEs — find the main FROM clause after any WITH ... AS (...) block.
	// Count parentheses to skip subqueries in CTE definitions.
	startIdx := 0
	if strings.HasPrefix(lower, "with ") {
		depth := 0
		for i := 0; i < len(lower); i++ {
			switch lower[i] {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			}
			// Look for "from " after CTE body closes (depth returns to 0).
			if depth == 0 && i > 0 {
				remaining := strings.TrimLeft(lower[i:], " 	\n\r,")
				if strings.HasPrefix(remaining, "from ") {
					startIdx = i + len(remaining) - len(strings.TrimPrefix(remaining, "from "))
					break
				}
			}
		}
	}

	// Search for FROM <table> after the start index.
	fromIdx := strings.Index(lower[startIdx:], "from ")
	if fromIdx >= 0 {
		afterFrom := strings.TrimSpace(lower[startIdx+fromIdx+5:])
		// Skip subquery: if it starts with "(", the table is not here.
		if len(afterFrom) > 0 && afterFrom[0] != '(' {
			return cleanTableName(extractIdentifier(afterFrom))
		}
	}

	// Fallback: UPDATE <table> SET ...
	updateIdx := strings.Index(lower[startIdx:], "update ")
	if updateIdx >= 0 {
		afterUpdate := strings.TrimSpace(lower[startIdx+updateIdx+7:])
		if len(afterUpdate) > 0 && afterUpdate[0] != '(' {
			return cleanTableName(extractIdentifier(afterUpdate))
		}
	}

	// INSERT INTO <table>
	insertIdx := strings.Index(lower[startIdx:], "insert into ")
	if insertIdx >= 0 {
		afterInsert := strings.TrimSpace(lower[startIdx+insertIdx+12:])
		if len(afterInsert) > 0 && afterInsert[0] != '(' {
			return cleanTableName(extractIdentifier(afterInsert))
		}
	}

	return "unknown"
}

// extractIdentifier extracts the first SQL identifier (table/column name) from a string.
func extractIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return ""
	}
	// Handle quoted identifiers.
	if s[0] == '"' {
		end := strings.Index(s[1:], "\"")
		if end >= 0 {
			return s[1 : end+1]
		}
		return s[1:]
	}
	// Unquoted identifier: alphanumeric + underscore, stops at space/comma/etc.
	var result strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '.' || c == '"' {
			result.WriteByte(c)
		} else {
			break
		}
	}
	return result.String()
}

// cleanTableName removes quotes and schema prefix from a table name.
func cleanTableName(name string) string {
	name = strings.Trim(name, "\"'")
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return "unknown"
	}
	return name
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
