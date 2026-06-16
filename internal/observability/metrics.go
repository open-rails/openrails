package observability

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// ObservabilityManager holds all metric collectors and their lifecycle.
type ObservabilityManager struct {
	config       MetricsConfig
	meter        metric.Meter
	shutdown     func(context.Context) error
	core         *Meter
	http         *HTTPMetrics
	runtime      *RuntimeMetrics
	db           *DBMetrics
	lastDBStats  sql.DBStats
	dbCollectCtx context.Context
	dbCollectCancel context.CancelFunc
}

// NewObservabilityManager initializes OTel metrics and all collectors.
func NewObservabilityManager(ctx context.Context, cfg MetricsConfig) (*ObservabilityManager, error) {
	if !cfg.Enabled {
		return &ObservabilityManager{
			config:   cfg,
			shutdown: func(context.Context) error { return nil },
		}, nil
	}

	// Create OTLP metric exporter.
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.DeploymentEnvironmentKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.ExportInterval))),
	)

	otel.SetMeterProvider(provider)

	// Create the global meter
	m := provider.Meter(cfg.ServiceName)

	// Create all metric collectors
	om := &ObservabilityManager{
		config:   cfg,
		meter:    m,
		shutdown: provider.Shutdown,
		core:     NewMeter(cfg.ServiceName),
		http:     NewHTTPMetrics(m),
		runtime:  NewRuntimeMetrics(m),
		db:       NewDBMetrics(m),
	}

	// Start runtime metrics collection
	om.runtime.Start(ctx, 10*time.Second)

	// Start DB pool stats collection
	om.dbCollectCtx, om.dbCollectCancel = context.WithCancel(context.Background())

	return om, nil
}

// Close shuts down all collectors and the OTel provider.
func (om *ObservabilityManager) Close(ctx context.Context) error {
	if om == nil {
		return nil
	}

	// Stop runtime collector
	if om.runtime != nil {
		om.runtime.Stop()
	}

	// Stop DB pool collector
	if om.dbCollectCancel != nil {
		om.dbCollectCancel()
	}

	// Shutdown OTel provider
	if om.shutdown != nil {
		return om.shutdown(ctx)
	}
	return nil
}

// HTTPMiddleware returns the HTTP metrics Gin middleware.
func (om *ObservabilityManager) HTTPMiddleware() gin.HandlerFunc {
	if om == nil || om.http == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return om.http.Middleware()
}

// DBPoolCollector collects DB pool stats periodically.
func (om *ObservabilityManager) DBPoolCollector() *DBMetrics {
	if om == nil || om.db == nil {
		return nil
	}
	return om.db
}

// SetDBInstance registers the bun DB instance for pool stats collection and query hooks.
func (om *ObservabilityManager) SetDBInstance(bunDB bun.IDB) {
	if om == nil || om.db == nil || bunDB == nil {
		return
	}

	// Start periodic pool stats collection
	go om.collectDBPoolLoop(bunDB)
}

func (om *ObservabilityManager) collectDBPoolLoop(bunDB bun.IDB) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-om.dbCollectCtx.Done():
			return
		case <-ticker.C:
			om.db.CollectPoolStats(om.dbCollectCtx, bunDB, &om.lastDBStats)
		}
	}
}
// Meter is a wrapper around the OTel Meter that provides pre-configured core instruments.
// This allows consistent metric naming and instrumentation across the codebase.
type Meter struct {
	meter      metric.Meter
	Latency    metric.Float64Histogram
	ErrCounter metric.Int64Counter
	MemoryUsage metric.Float64Gauge
}

// Meter returns a new *Meter bound to this manager's meter provider.
// Call this AFTER NewObservabilityManager to get instruments that actually
// send data to the configured provider.
func (om *ObservabilityManager) Meter(name string) *Meter {
	if om == nil || om.meter == nil {
		return NewNoopMeter()
	}
	return newMeter(om.meter, name)
}

// NewMeter creates a new Meter and initializes its core instruments.
// Deprecated: Use ObservabilityManager.Meter(name) instead. This function
// captures otel.GetMeterProvider() at call time, which is the no-op provider
// before InitTelemetry runs, resulting in silent metric loss.
func NewMeter(name string) *Meter {
	m := otel.GetMeterProvider().Meter(name)
	return newMeter(m, name)
}

// newMeter builds a *Meter from a concrete metric.Meter instance.
// This is the shared implementation that both Meter() and NewMeter use.
func newMeter(m metric.Meter, name string) *Meter {
	latency, err := m.Float64Histogram(
		"core_function_latency_seconds",
		metric.WithDescription("Latency of critical core functions"),
		metric.WithUnit("s"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "core_function_latency_seconds").Error("failed to create OTel histogram")
	}

	errCounter, err := m.Int64Counter(
		"core_function_errors_total",
		metric.WithDescription("Total count of errors in critical core functions"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "core_function_errors_total").Error("failed to create OTel counter")
	}

	memoryUsage, err := m.Float64Gauge(
		"core_function_memory_usage_bytes",
		metric.WithDescription("Memory usage of critical core functions"),
		metric.WithUnit("B"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "core_function_memory_usage_bytes").Error("failed to create OTel gauge")
	}

	return &Meter{
		meter:       m,
		Latency:     latency,
		ErrCounter:  errCounter,
		MemoryUsage: memoryUsage,
	}
}

// NewNoopMeter returns a Meter backed by the no-op provider.
// Used when the ObservabilityManager is nil or metrics are disabled.
func NewNoopMeter() *Meter {
	return newMeter(noop.Meter{}, "noop")
}

// InitMetrics initializes the OpenTelemetry metrics provider.
// Deprecated: Use NewObservabilityManager instead.
func InitMetrics(ctx context.Context, cfg MetricsConfig) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	// Create OTLP metric exporter.
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.DeploymentEnvironmentKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.ExportInterval))),
	)

	otel.SetMeterProvider(provider)

	return provider.Shutdown, nil
}
