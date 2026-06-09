package observability

import (
	"os"
	"strconv"
	"time"
)

// MetricsConfig holds the configuration for OpenTelemetry metrics.
type MetricsConfig struct {
	Enabled        bool
	ServiceName    string
	OTLPEndpoint   string
	OTLPProtocol   string
	ExportInterval time.Duration
	Environment    string
}

// LoadMetricsConfig reads metrics configuration from environment variables.
func LoadMetricsConfig() MetricsConfig {
	return MetricsConfig{
		Enabled:        os.Getenv("OTEL_METRICS_ENABLED") == "true",
		ServiceName:    getEnv("OTEL_SERVICE_NAME", "openrails-service"),
		OTLPEndpoint:   getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"),
		OTLPProtocol:   getEnv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"),
		ExportInterval: parseDuration(getEnv("OTEL_METRIC_EXPORT_INTERVAL", "10s")),
		Environment:    getEnv("OTEL_ENVIRONMENT", "local"),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 10 * time.Second
	}
	return d
}
