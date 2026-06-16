package observability

import (
	"context"
	"runtime"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"
)

// RuntimeMetrics collects Go runtime metrics (goroutines, memory, GC).
// It runs a periodic collector that records metrics at the given interval.
type RuntimeMetrics struct {
	goroutines   metric.Int64Gauge
	memAlloc     metric.Int64Gauge
	memHeapInUse metric.Int64Gauge
	memHeapSys   metric.Int64Gauge
	memStackInUse metric.Int64Gauge
	gcPauseTotal metric.Float64Counter
	gcCycles     metric.Int64Counter
	stopCh       chan struct{}
}

// NewRuntimeMetrics creates runtime metrics instruments. Does NOT start collecting yet.
func NewRuntimeMetrics(meter metric.Meter) *RuntimeMetrics {
	r := &RuntimeMetrics{
		stopCh: make(chan struct{}),
	}

	var err error
	r.goroutines, err = meter.Int64Gauge(
		"go_goroutines",
		metric.WithDescription("Number of goroutines"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_goroutines").Error("failed to create OTel gauge")
	}

	r.memAlloc, err = meter.Int64Gauge(
		"go_memstats_alloc_bytes",
		metric.WithDescription("Number of bytes allocated and not yet freed"),
		metric.WithUnit("By"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_alloc_bytes").Error("failed to create OTel gauge")
	}

	r.memHeapInUse, err = meter.Int64Gauge(
		"go_memstats_heap_inuse_bytes",
		metric.WithDescription("Number of bytes in heap in-use spans"),
		metric.WithUnit("By"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_heap_inuse_bytes").Error("failed to create OTel gauge")
	}

	r.memHeapSys, err = meter.Int64Gauge(
		"go_memstats_heap_sys_bytes",
		metric.WithDescription("Number of bytes obtained from system (heap)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_heap_sys_bytes").Error("failed to create OTel gauge")
	}

	r.memStackInUse, err = meter.Int64Gauge(
		"go_memstats_stack_inuse_bytes",
		metric.WithDescription("Number of bytes in stack in-use spans"),
		metric.WithUnit("By"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_stack_inuse_bytes").Error("failed to create OTel gauge")
	}

	r.gcPauseTotal, err = meter.Float64Counter(
		"go_memstats_gc_pause_total_seconds",
		metric.WithDescription("Total pause duration for GC"),
		metric.WithUnit("s"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_gc_pause_total_seconds").Error("failed to create OTel counter")
	}

	r.gcCycles, err = meter.Int64Counter(
		"go_memstats_num_gc_cycles_total",
		metric.WithDescription("Number of GC cycles"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.WithError(err).WithField("instrument", "go_memstats_num_gc_cycles_total").Error("failed to create OTel counter")
	}

	return r
}

// Start begins periodic collection of runtime metrics.
func (r *RuntimeMetrics) Start(ctx context.Context, interval time.Duration) {
	go r.collectLoop(ctx, interval)
}

// Stop signals the collector to stop.
func (r *RuntimeMetrics) Stop() {
	close(r.stopCh)
}

func (r *RuntimeMetrics) collectLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.collect(ctx)
		}
	}
}

func (r *RuntimeMetrics) collect(ctx context.Context) {
	if r == nil {
		return
	}

	// Goroutines
	if r.goroutines != nil {
		r.goroutines.Record(ctx, int64(runtime.NumGoroutine()))
	}

	// Memory stats
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	if r.memAlloc != nil {
		r.memAlloc.Record(ctx, int64(m.Alloc))
	}
	if r.memHeapInUse != nil {
		r.memHeapInUse.Record(ctx, int64(m.HeapInuse))
	}
	if r.memHeapSys != nil {
		r.memHeapSys.Record(ctx, int64(m.HeapSys))
	}
	if r.memStackInUse != nil {
		r.memStackInUse.Record(ctx, int64(m.StackInuse))
	}

	// GC stats
	if r.gcPauseTotal != nil {
		r.gcPauseTotal.Add(ctx, float64(m.PauseTotalNs)/1e9)
	}
	if r.gcCycles != nil {
		r.gcCycles.Add(ctx, int64(m.NumGC))
	}
}
