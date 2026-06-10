package platform

import (
	"context"
	"net/http"
	"sync"

	"github.com/open-rails/openrails/internal/observability"
)

var (
	once sync.Once
)

// InitTelemetry initializes the OpenTelemetry metrics provider and all collectors.
func InitTelemetry() (*observability.ObservabilityManager, error) {
	cfg := observability.LoadMetricsConfig()
	om, err := observability.NewObservabilityManager(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return om, nil
}

// MetricsHandler returns the Prometheus metrics handler.
// Note: In this architecture, metrics are served by the OpenTelemetry Collector.
func MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Metrics are served by the OTel Collector"))
	})
}