package app

import (
	"context"
	"fmt"
	"slices"

	riverhelpers "github.com/open-rails/helpers/river"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
)

func (r *Runtime) riverConfigurableLocked() error {
	if r.riverClosed.Load() {
		return fmt.Errorf("runtime is closed")
	}
	if r.riverCompositionSealed || r.RiverClient != nil || r.externalRiverClient {
		return fmt.Errorf("River composition is already sealed: attach components before requesting RiverJobs")
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

// RiverJobs seals the attached component set before handing it to the host.
// Request it only after optional control-plane attachment. No client is built
// and no job starts until the host composes and starts the returned group.
func (r *Runtime) RiverJobs() riverhelpers.Contribution { return r.riverJobs(true) }
func (r *Runtime) riverJobs(host bool) riverhelpers.Contribution {
	refuse := func(err error) riverhelpers.Contribution {
		return riverhelpers.NewContribution("openrails", func(context.Context, *river.Config) error { return err }, nil, nil)
	}
	if r == nil {
		return refuse(fmt.Errorf("runtime is nil"))
	}
	r.riverCompositionMu.Lock()
	if err := r.riverConfigurableLocked(); err != nil {
		r.riverCompositionMu.Unlock()
		return refuse(err)
	}
	if r.hostRiver != host {
		r.riverCompositionMu.Unlock()
		return refuse(fmt.Errorf("host composition requires RiverFromHost ownership"))
	}
	r.riverCompositionSealed = true
	components := slices.Clone(r.riverContributions)
	r.riverCompositionMu.Unlock()
	claimed := false
	own := riverhelpers.NewContribution("openrails", func(ctx context.Context, cfg *river.Config) error {
		r.riverCompositionMu.Lock()
		if r.riverClosed.Load() || r.riverCompositionFailed {
			r.riverCompositionMu.Unlock()
			return fmt.Errorf("runtime is closed or River composition failed")
		}
		claimed = true
		r.riverCompositionMu.Unlock()
		queues := map[string]int{riverjobs.QueueBilling: standaloneRiverBillingQueueMaxWorkers}
		refreshQueue := riverjobs.QueueBilling
		if !host {
			queues[river.QueueDefault] = standaloneRiverDefaultQueueMaxWorkers
			queues[riverjobs.QueueProviderRefresh] = standaloneRiverProviderRefreshQueueMaxWorkers
			refreshQueue = riverjobs.QueueProviderRefresh
		}
		for name, count := range queues {
			if _, present := cfg.Queues[name]; !present {
				cfg.Queues[name] = river.QueueConfig{MaxWorkers: count}
			}
			if cfg.Queues[name].MaxWorkers < 1 {
				return fmt.Errorf("required River queue %q is disabled", name)
			}
		}
		if cfg.JobTimeout == 0 {
			cfg.JobTimeout = riverNoJobTimeout
		}
		r.ProviderRefreshQueue = refreshQueue
		if err := r.addBillingWorkersToRegistry(ctx, cfg.Workers, refreshQueue); err != nil {
			return err
		}
		periodic, err := r.buildRiverPeriodicJobs(ctx)
		if err != nil {
			return err
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, periodic...)
		return nil
	}, func(ctx context.Context, binding riverhelpers.Binding) error {
		client := binding.Client
		if err := r.DB.ValidateRiverJobBinding(ctx, binding.Pool, client.Schema()); err != nil {
			return err
		}
		r.riverCompositionMu.Lock()
		if r.riverClosed.Load() || r.riverCompositionFailed {
			r.riverCompositionMu.Unlock()
			return fmt.Errorf("runtime closed during River composition")
		}
		r.SetRiverSchema(client.Schema())
		r.RiverClient = client
		r.DB.SetRiverJobInserter(client)
		if host {
			r.RiverProducer = client
			r.externalRiverClient = true
			r.hostRiverBound.Store(true)
		}
		// Close must not consume the monitor's stop-once before its start hook.
		r.StartRiverProgressMonitor(ctx)
		r.riverCompositionMu.Unlock()
		return nil
	}, func() error {
		if !claimed {
			return nil
		}
		r.riverCompositionMu.Lock()
		if r.riverClosed.Load() {
			r.riverCompositionMu.Unlock()
			return nil
		}
		r.riverCompositionFailed = true
		r.RiverClient = nil
		r.DB.SetRiverJobInserter(nil)
		if host {
			r.RiverProducer = nil
			r.externalRiverClient = false
			r.hostRiverBound.Store(false)
		}
		r.riverCompositionMu.Unlock()
		r.stopRiverProgressMonitor()
		return nil
	})
	return riverhelpers.Group(append([]riverhelpers.Contribution{own}, components...)...)
}
