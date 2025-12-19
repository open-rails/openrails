package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	riverjobs "github.com/doujins-org/doujins-billing/internal/river"
)

// buildRiverWorkers constructs the worker registry for River.
func (r *Runtime) buildRiverWorkers(ctx context.Context) (*river.Workers, error) {
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &riverjobs.DunningWorker{DB: r.DB, NMIClients: r.NMIClients, EventLogService: r.EventLogService, IdempotencyService: r.IdempotencyService}); err != nil {
		return nil, fmt.Errorf("add dunning worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.IdempotencyCleanupWorker{DB: r.DB}); err != nil {
		return nil, fmt.Errorf("add idempotency cleanup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CCBillReconcileWorker{DB: r.DB, DataLink: r.CCBillDataLink}); err != nil {
		return nil, fmt.Errorf("add ccbill reconcile worker: %w", err)
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
		return nil, fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CreditExpiryWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return nil, fmt.Errorf("add credit expiry worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CancelSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		UserSubscriptionService:      r.UserSubscriptionService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return nil, fmt.Errorf("add cancel subscription worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.ResumeSubscriptionWorker{
		DB:                  r.DB,
		Config:              r.Config,
		EntitlementService:  r.EntitlementService,
		SubscriptionService: r.SubscriptionService,
	}); err != nil {
		return nil, fmt.Errorf("add resume subscription worker: %w", err)
	}
	return workers, nil
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

	return jobs, nil
}
