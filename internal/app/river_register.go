package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// buildRiverWorkers constructs the worker registry for River.
func (r *Runtime) buildRiverWorkers(ctx context.Context) (*river.Workers, error) {
	workers := river.NewWorkers()
	if err := r.addBillingWorkersToRegistry(ctx, workers); err != nil {
		return nil, err
	}
	return workers, nil
}

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// This is used both internally (buildRiverWorkers) and externally (AddBillingWorkersTo).
func (r *Runtime) addBillingWorkersToRegistry(ctx context.Context, workers *river.Workers) error {
	if err := r.validateBillingWorkerRuntime(); err != nil {
		return err
	}

	if err := river.AddWorkerSafely(workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, NMIClients: r.NMIClients, EventLogService: r.EventLogService, IdempotencyService: r.IdempotencyService}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.IdempotencyCleanupWorker{}); err != nil {
		return fmt.Errorf("add idempotency cleanup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CCBillReconcileWorker{DB: r.DB, DataLink: r.CCBillDataLink}); err != nil {
		return fmt.Errorf("add ccbill reconcile worker: %w", err)
	}
	// Webhook processing is now synchronous-only - no background workers needed.
	// Payment processors (CCBill, NMI) retry failed webhooks from their end.
	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CleanupExpiredDataWorker{
		DB:     r.DB,
		Clock:  clock,
		Config: riverjobs.DefaultCleanupConfig(),
	}); err != nil {
		return fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CreditExpiryWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add credit expiry worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.HoldExpiryWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add hold expiry worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CancelSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		UserSubscriptionService:      r.UserSubscriptionService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add cancel subscription worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.ResumeSubscriptionWorker{
		DB:                  r.DB,
		Config:              r.Config,
		EntitlementService:  r.EntitlementService,
		SubscriptionService: r.SubscriptionService,
	}); err != nil {
		return fmt.Errorf("add resume subscription worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.WebhookProcessWorker{
		Dispatcher: r.WebhookDispatcher,
	}); err != nil {
		return fmt.Errorf("add webhook process worker: %w", err)
	}
	return nil
}

func (r *Runtime) validateBillingWorkerRuntime() error {
	if r == nil {
		return fmt.Errorf("runtime is required")
	}
	if r.DB == nil {
		return fmt.Errorf("billing worker runtime DB is required")
	}
	if r.Config == nil {
		return fmt.Errorf("billing worker runtime config is required")
	}
	if r.SubscriptionService == nil {
		return fmt.Errorf("billing worker runtime subscription service is required")
	}
	if r.UserSubscriptionService == nil {
		return fmt.Errorf("billing worker runtime user subscription service is required")
	}
	if r.SubscriptionLifecycleService == nil {
		return fmt.Errorf("billing worker runtime subscription lifecycle service is required")
	}
	if r.EntitlementService == nil {
		return fmt.Errorf("billing worker runtime entitlement service is required")
	}
	if r.WebhookDispatcher == nil {
		return fmt.Errorf("billing worker runtime webhook dispatcher is required")
	}
	return nil
}

// buildRiverPeriodicJobs defines recurring schedules for workers using River periodic jobs.
func (r *Runtime) buildRiverPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error) {
	var jobs []*river.PeriodicJob

	// Every 4 hours: run Dunning worker to process past_due subscriptions
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(4*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.DunningArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Daily: Idempotency cleanup
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.IdempotencyCleanupArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 6 hours: CCBill reconcile
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(6*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CCBillReconcileArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Webhook retry job removed - webhooks are now processed synchronously only.
	// Payment processors (CCBill, NMI) will retry failed webhooks from their end.

	// Every hour: cleanup expired data (wallet challenges, payment intents, etc.)
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: expire credit batches
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CreditExpiryArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 5 minutes: expire orphaned credit holds
	// Handles cases where jobs crash without calling capture/release
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.HoldExpiryArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	return jobs, nil
}
