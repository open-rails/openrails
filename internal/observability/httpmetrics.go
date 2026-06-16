package observability

import (
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// HTTPMetrics tracks HTTP request metrics via Gin middleware.
type HTTPMetrics struct {
	requestCounter   metric.Int64Counter
	requestDuration  metric.Float64Histogram
	activeGauge      metric.Int64Gauge
	requestBodySize  metric.Float64Histogram
	responseBodySize metric.Float64Histogram
}

// NewHTTPMetrics creates the HTTP metrics instruments.
func NewHTTPMetrics(meter metric.Meter) *HTTPMetrics {
	h := &HTTPMetrics{}

	var err error
	h.requestCounter, err = meter.Int64Counter(
		"http_requests_total",
		metric.WithDescription("Total count of HTTP requests"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "http_requests_total").Error("failed to create OTel counter")
	}

	h.requestDuration, err = meter.Float64Histogram(
		"http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "http_request_duration_seconds").Error("failed to create OTel histogram")
	}

	h.activeGauge, err = meter.Int64Gauge(
		"http_active_connections",
		metric.WithDescription("Current number of active HTTP connections"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "http_active_connections").Error("failed to create OTel gauge")
	}

	h.requestBodySize, err = meter.Float64Histogram(
		"http_request_size_bytes",
		metric.WithDescription("HTTP request body size in bytes"),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(100, 1000, 10000, 100000, 1000000),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "http_request_size_bytes").Error("failed to create OTel histogram")
	}

	h.responseBodySize, err = meter.Float64Histogram(
		"http_response_size_bytes",
		metric.WithDescription("HTTP response body size in bytes"),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(100, 1000, 10000, 100000, 1000000),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "http_response_size_bytes").Error("failed to create OTel histogram")
	}

	return h
}

// Middleware returns a Gin handler that records HTTP metrics.
func (h *HTTPMetrics) Middleware() gin.HandlerFunc {
	if h == nil {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		start := time.Now()

		// Track active connections
		if h.activeGauge != nil {
			h.activeGauge.Record(c.Request.Context(), 1)
			defer func() {
				h.activeGauge.Record(c.Request.Context(), -1)
			}()
		}

		// Process request
		c.Next()

		// Determine the route pattern (not the raw path with params)
		route := c.FullPath()
		if route == "" {
			// Unmatched route — collapse to "/unknown" to prevent cardinality
			// explosions from user-controlled paths (e.g. /api/users/123,
			// /api/users/456, /uploads/abc123.jpg, etc.).
			route = "/unknown"
		}

		// Record metrics
		attrs := []attribute.KeyValue{
			attribute.String("method", c.Request.Method),
			attribute.String("path", route),
			attribute.String("status", strconv.Itoa(c.Writer.Status())),
		}

		duration := time.Since(start).Seconds()
		if h.requestDuration != nil {
			h.requestDuration.Record(c.Request.Context(), duration, metric.WithAttributes(attrs...))
		}

		if h.requestCounter != nil {
			h.requestCounter.Add(c.Request.Context(), 1, metric.WithAttributes(attrs...))
		}

		// Body sizes
		if h.requestBodySize != nil {
			contentLen := c.Request.ContentLength
			if contentLen < 0 {
				contentLen = 0
			}
			h.requestBodySize.Record(c.Request.Context(), float64(contentLen), metric.WithAttributes(attrs...))
		}
		if h.responseBodySize != nil {
			h.responseBodySize.Record(c.Request.Context(), float64(c.Writer.Size()), metric.WithAttributes(attrs...))
		}
	}
}
