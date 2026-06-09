package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Meter is a wrapper around the OTel Meter to provide easy access to core instruments.
type Meter struct {
	m           metric.Meter
	latency     metric.Float64Histogram
	errCounter  metric.Int64Counter
	memoryUsage metric.Float64Gauge
}

// NewMeter creates a new Meter and initializes its core instruments.
func NewMeter(name string) *Meter {
	m, _ := otel.GetMeterProvider().Meter(name)
	
	latency, _ := m.Float64Histogram(
		"core_function_latency_seconds",
		metric.WithDescription("Latency of critical core functions"),
		metric.WithUnit("s"),
	)
	
	errCounter, _ := m.Int64Counter(
		"core_function_errors_total",
		metric.WithDescription("Total count of errors in critical core functions"),
	)
	
	memoryUsage, _ := m.Float64Gauge(
		"core_function_memory_usage_bytes",
		metric.WithDescription("Memory usage of critical core functions"),
		metric.WithUnit("B"),
	)

	return &Meter{
		m:           m,
		latency:     latency,
		errCounter:  errCounter,
		memoryUsage: memoryUsage,
	}
}

// InitMetrics initializes the OpenTelemetry metrics provider.
// Returns a shutdown function to be called on application exit.
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
