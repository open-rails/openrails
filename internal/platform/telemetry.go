package platform

import (
	"context"
	"net/http"
	"sync"

	"github.com/open-rails/openrails/internal/observability"
)

var (
	once       sync.Once
	meter      observability.Meter
	latency    observability.Float64Histogram
	errCounter observability.Int64Counter
	memory     observability.Float64Gauge
)

// InitTelemetry initializes the OpenTelemetry metrics provider.
func InitTelemetry() (observability.Meter, error) {
	_, _ = observability.InitMetrics(context.Background(), observability.LoadMetricsConfig())
	once.Do(func() {
		// The actual meter is registered via InitMetrics
	})
	return meter, nil
}

// MetricsHandler returns the Prometheus metrics handler.
// Note: In this architecture, this is a placeholder as metrics are served 
// by the OpenTelemetry Collector, not directly by the application.
func MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Metrics are served by the OTel Collector"))
	})
}

// RegisterCoreMetrics is a helper to ensure metrics are initialized.
func RegisterCoreMetrics(ctx context.Context) (
	latency observability.Float64Histogram,
	errCounter observability.Int64Counter,
	memory observability.Float64Gauge,
) {
	_, _ = InitTelemetry()
	// We need to fetch the actual instruments from the meter
	// This will be updated once the observability package provides a way to fetch them
	return latency, errCounter, memory
}