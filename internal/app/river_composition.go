package app

import (
	"context"
	"fmt"
	"slices"

	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
)

func (r *Runtime) riverConfigurableLocked() error {
	if r.riverClosed.Load() {
		return fmt.Errorf("runtime is closed")
	}
	if r.riverCompositionSealed || r.RiverClient != nil || r.externalRiverClient {
		return fmt.Errorf("River composition is already bound: attach components before initialization")
	}
	return nil
}

func (r *Runtime) CheckRiverConfigurable() error {
	if r == nil {
		return fmt.Errorf("runtime is nil")
	}
	r.riverCompositionMu.Lock()
	defer r.riverCompositionMu.Unlock()
	return r.riverConfigurableLocked()
}

// PrepareHostRiverConfig seals one startup attempt before any component can
// register twice. A failed attempt is fatal setup; recreate the runtime.
func (r *Runtime) PrepareHostRiverConfig(ctx context.Context) (*river.Config, error) {
	r.riverCompositionMu.Lock()
	if !r.hostRiver {
		r.riverCompositionMu.Unlock()
		return nil, fmt.Errorf("BindRiver requires RiverFromHost ownership")
	}
	if err := r.riverConfigurableLocked(); err != nil {
		r.riverCompositionMu.Unlock()
		return nil, err
	}
	r.riverCompositionSealed = true
	configure := slices.Clone(r.riverConfigurers)
	r.riverCompositionMu.Unlock()
	workers := river.NewWorkers()
	if err := r.AddBillingWorkersTo(ctx, workers); err != nil {
		return nil, fmt.Errorf("register billing workers: %w", err)
	}
	periodic, err := r.GetBillingPeriodicJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("build billing periodic jobs: %w", err)
	}
	cfg := &river.Config{
		Schema: r.riverSchemaOrDefault(), Workers: workers, JobTimeout: riverNoJobTimeout,
		Queues:       map[string]river.QueueConfig{riverjobs.QueueBilling: {MaxWorkers: standaloneRiverBillingQueueMaxWorkers}},
		PeriodicJobs: periodic,
	}
	for _, fn := range configure {
		if err := fn(ctx, cfg); err != nil {
			return nil, fmt.Errorf("configure River component: %w", err)
		}
	}
	return cfg, nil
}
