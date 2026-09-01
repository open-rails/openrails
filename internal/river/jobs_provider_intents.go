package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
)

// workerNow reads the worker's clock (real when unset).
func workerNow(c clockwork.Clock) time.Time {
	if c != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

const (
	KindProviderIntentExecute = "openrails.provider_intent_execute"
	KindProviderIntentVerify  = "openrails.provider_intent_verify"
)

// ProviderIntentExecuteArgs triggers one executor pass over the provider
// intent ledger (#358): expire overdue intents, claim due ones (SKIP LOCKED
// lease), gate on mode x origin, execute, classify outcomes. This is the
// ACTION pipeline — deliberately a scheduled worker (the no-recurring-jobs
// ruling applies to reconcile RUNS, not to intent execution).
type ProviderIntentExecuteArgs struct{}

func (ProviderIntentExecuteArgs) Kind() string { return KindProviderIntentExecute }

// ProviderIntentVerifyArgs triggers one verifier pass: resolve
// unknown_needs_verify intents via provider READS before any retry.
type ProviderIntentVerifyArgs struct{}

func (ProviderIntentVerifyArgs) Kind() string { return KindProviderIntentVerify }

// ProviderIntentExecuteWorker drains the executable intents.
type ProviderIntentExecuteWorker struct {
	river.WorkerDefaults[ProviderIntentExecuteArgs]
	DB             *db.DB
	Config         *config.Config
	Clock          clockwork.Clock
	Registry       *intents.Registry
	MutationLogger intents.MutationLogger
}

func (ProviderIntentExecuteWorker) Kind() string { return KindProviderIntentExecute }

func (w ProviderIntentExecuteWorker) Work(ctx context.Context, _ *river.Job[ProviderIntentExecuteArgs]) error {
	if w.DB == nil || w.Registry == nil {
		return fmt.Errorf("provider intent executor: DB and registry are required")
	}
	store := intents.NewStore(w.DB)
	runner := &intents.Runner{
		Store:    store,
		Logger:   w.MutationLogger,
		Registry: w.Registry,
		Config:   w.Config,
		// #679: destructive types (nmi_delete_subscription) are volume-gated
		// per merchant; over-budget intents park until an operator resolves
		// the life.provider_intent.held_bulk finding.
		Breaker: intents.NewVolumeBreaker(w.DB),
		// #836: the DB-backed operator kill switch, checked before every
		// destructive provider write.
		Destructive: destructive.New(w.DB),
		Clock:       w.Clock,
	}
	logger := log.WithContext(ctx).WithField("worker", KindProviderIntentExecute)
	now := workerNow(w.Clock)

	// or#862: this used to call RunExecuteOnce on the BARE job context, which is
	// how the whole outbound provider-mutation plane came to be inert. There is
	// no privileged pool: with no app.merchant_id, rail_intents' FORCEd RLS made
	// the claim match `merchant_id = NULL` and lease ZERO intents — silently —
	// so the #836 kill switch and the #679 volume breaker, which only ever run
	// on a claimed intent, never executed either. Enumerate the merchants with
	// due work through 0022's SECURITY DEFINER work queue (ids only, and it
	// RAISES if its definer cannot bypass RLS), then run each merchant's pass
	// inside that merchant's own scope, like every other worker in this package.
	merchantIDs, err := store.DueExecuteMerchants(ctx, now, providerIntentMerchantFanout)
	if err != nil {
		return fmt.Errorf("provider intent executor: list merchants with due intents: %w", err)
	}

	var total intents.Stats
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "intent executor merchant "+mid.String())
		var stats intents.Stats
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(mid), "provider intent executor", func(ctx context.Context) error {
			var e error
			stats, e = runner.RunExecuteOnce(ctx)
			return e
		}); err != nil {
			// One merchant's failure must not abort the rest of the fan-out.
			logger.WithError(err).WithField("merchant_id", mid).
				Error("provider intent executor: merchant pass failed; continuing")
			continue
		}
		total.Add(stats)
	}
	if total.Claimed > 0 || total.Expired > 0 {
		logger.WithFields(log.Fields{
			"merchants":  len(merchantIDs),
			"claimed":    total.Claimed,
			"succeeded":  total.Succeeded,
			"retryable":  total.Retryable,
			"unknown":    total.Unknown,
			"terminal":   total.Terminal,
			"parked":     total.Parked,
			"superseded": total.Superseded,
			"expired":    total.Expired,
		}).Info("Provider intent executor: pass completed")
	}
	return nil
}

// providerIntentMerchantFanout bounds how many merchants one executor/verifier
// pass visits. Work scales with ACTIVITY, not with the merchant roster: the
// 0022 work queues only surface merchants that actually have a due intent, so
// this caps a burst, it is not a scan budget.
const providerIntentMerchantFanout = 500

// ProviderIntentVerifyWorker resolves ambiguous outcomes via provider reads.
type ProviderIntentVerifyWorker struct {
	river.WorkerDefaults[ProviderIntentVerifyArgs]
	DB             *db.DB
	Config         *config.Config
	Clock          clockwork.Clock
	Registry       *intents.Registry
	MutationLogger intents.MutationLogger
}

func (ProviderIntentVerifyWorker) Kind() string { return KindProviderIntentVerify }

func (w ProviderIntentVerifyWorker) Work(ctx context.Context, _ *river.Job[ProviderIntentVerifyArgs]) error {
	if w.DB == nil || w.Registry == nil {
		return fmt.Errorf("provider intent verifier: DB and registry are required")
	}
	store := intents.NewStore(w.DB)
	runner := &intents.Runner{
		Store:    store,
		Logger:   w.MutationLogger,
		Registry: w.Registry,
		Config:   w.Config,
		Clock:    w.Clock,
	}
	logger := log.WithContext(ctx).WithField("worker", KindProviderIntentVerify)
	now := workerNow(w.Clock)

	// or#862: same fan-out as the executor. An unverified ambiguous intent is
	// never retried, so a blind verifier plane strands every unknown outcome.
	merchantIDs, err := store.DueVerifyMerchants(ctx, now, providerIntentMerchantFanout)
	if err != nil {
		return fmt.Errorf("provider intent verifier: list merchants with due verifications: %w", err)
	}

	var total intents.Stats
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "intent verifier merchant "+mid.String())
		var stats intents.Stats
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(mid), "provider intent verifier", func(ctx context.Context) error {
			var e error
			stats, e = runner.RunVerifyOnce(ctx)
			return e
		}); err != nil {
			logger.WithError(err).WithField("merchant_id", mid).
				Error("provider intent verifier: merchant pass failed; continuing")
			continue
		}
		total.Add(stats)
	}
	if total.Claimed > 0 {
		logger.WithFields(log.Fields{
			"merchants":  len(merchantIDs),
			"claimed":    total.Claimed,
			"succeeded":  total.Succeeded,
			"retryable":  total.Retryable,
			"unknown":    total.Unknown,
			"superseded": total.Superseded,
		}).Info("Provider intent verifier: pass completed")
	}
	return nil
}
