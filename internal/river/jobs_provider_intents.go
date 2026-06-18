package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
)

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
	DB       *db.DB
	Config   *config.Config
	Clock    clockwork.Clock
	Registry *intents.Registry
	// ProviderAccounts gates execution on the provider account (#518).
	ProviderAccounts intents.ProviderAccountResolver
}

func (ProviderIntentExecuteWorker) Kind() string { return KindProviderIntentExecute }

func (w ProviderIntentExecuteWorker) Work(ctx context.Context, _ *river.Job[ProviderIntentExecuteArgs]) error {
	if w.DB == nil || w.Registry == nil {
		return fmt.Errorf("provider intent executor: DB and registry are required")
	}
	runner := &intents.Runner{
		Store:            intents.NewStore(w.DB),
		Registry:         w.Registry,
		Config:           w.Config,
		Clock:            w.Clock,
		ProviderAccounts: w.ProviderAccounts,
	}
	stats, err := runner.RunExecuteOnce(ctx)
	if err != nil {
		return fmt.Errorf("provider intent executor: %w", err)
	}
	if stats.Claimed > 0 || stats.Expired > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"claimed":    stats.Claimed,
			"succeeded":  stats.Succeeded,
			"retryable":  stats.Retryable,
			"unknown":    stats.Unknown,
			"terminal":   stats.Terminal,
			"parked":     stats.Parked,
			"superseded": stats.Superseded,
			"expired":    stats.Expired,
		}).Info("Provider intent executor: pass completed")
	}
	return nil
}

// ProviderIntentVerifyWorker resolves ambiguous outcomes via provider reads.
type ProviderIntentVerifyWorker struct {
	river.WorkerDefaults[ProviderIntentVerifyArgs]
	DB       *db.DB
	Config   *config.Config
	Clock    clockwork.Clock
	Registry *intents.Registry
	// ProviderAccounts defers verification on provider-account mismatch (#518).
	ProviderAccounts intents.ProviderAccountResolver
}

func (ProviderIntentVerifyWorker) Kind() string { return KindProviderIntentVerify }

func (w ProviderIntentVerifyWorker) Work(ctx context.Context, _ *river.Job[ProviderIntentVerifyArgs]) error {
	if w.DB == nil || w.Registry == nil {
		return fmt.Errorf("provider intent verifier: DB and registry are required")
	}
	runner := &intents.Runner{
		Store:            intents.NewStore(w.DB),
		Registry:         w.Registry,
		Config:           w.Config,
		Clock:            w.Clock,
		ProviderAccounts: w.ProviderAccounts,
	}
	stats, err := runner.RunVerifyOnce(ctx)
	if err != nil {
		return fmt.Errorf("provider intent verifier: %w", err)
	}
	if stats.Claimed > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"claimed":    stats.Claimed,
			"succeeded":  stats.Succeeded,
			"retryable":  stats.Retryable,
			"unknown":    stats.Unknown,
			"superseded": stats.Superseded,
		}).Info("Provider intent verifier: pass completed")
	}
	return nil
}
