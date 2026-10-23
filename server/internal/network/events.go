package network

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const eventPollInterval = time.Minute
const eventPollKind = "openrails.network_read_account_events"

type accountEventsArgs struct{}

func (accountEventsArgs) Kind() string { return eventPollKind }

type accountEventsWorker struct {
	river.WorkerDefaults[accountEventsArgs]
	service *Service
}

// One bounded read; the next periodic tick retries failures without building
// a parallel chain of delayed source-consumer jobs.
func (*accountEventsWorker) Timeout(*river.Job[accountEventsArgs]) time.Duration {
	return 30 * time.Second
}

func (w *accountEventsWorker) Work(ctx context.Context, _ *river.Job[accountEventsArgs]) error {
	_, err := w.service.PollEvents(ctx)
	return err
}

// RiverJobs contributes one bounded reader only when the customer network is
// enabled. Progress belongs to this server, never to the AuthKit event source.
func (s *Service) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("openrails-network", func(_ context.Context, cfg *river.Config) error {
		if cfg.Workers == nil {
			cfg.Workers = river.NewWorkers()
		}
		if err := river.AddWorkerSafely(cfg.Workers, &accountEventsWorker{service: s}); err != nil {
			return err
		}
		if cfg.Queues == nil {
			cfg.Queues = map[string]river.QueueConfig{}
		}
		const queue = "billing"
		if q, ok := cfg.Queues[queue]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("network: billing queue is disabled")
		}
		if _, ok := cfg.Queues[queue]; !ok {
			cfg.Queues[queue] = river.QueueConfig{MaxWorkers: 1}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(eventPollInterval), func() (river.JobArgs, *river.InsertOpts) {
			return accountEventsArgs{}, &river.InsertOpts{Queue: queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByQueue: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
		}, &river.PeriodicJobOpts{ID: eventPollKind, RunOnStart: true}))
		return nil
	}, nil, nil)
}

// PollEvents applies at most one page of committed account facts. The page is
// fetched before acquiring the billing transaction so one-connection pools are
// supported. Effects and the server's cursor commit together; the row lock/CAS
// prevents two instances from applying older facts after a newer page.
// A lost source cursor is an explicit error requiring operator resynchronization.
func (s *Service) PollEvents(ctx context.Context) (int, error) {
	source, err := s.ak.ForTenant(ctx, s.tenant)
	if err != nil {
		return 0, fmt.Errorf("network: select account tenant: %w", err)
	}
	scoped := *s
	scoped.ak = source
	return scoped.pollEvents(ctx, source.ListEvents)
}

func (s *Service) pollEvents(ctx context.Context, read func(context.Context, iam.EventQuery) (iam.EventPage, error)) (int, error) {
	key := gen.GetNetworkEventCursorParams{Issuer: s.issuer, TenantID: uuid.UUID(s.tenant)}
	before, err := s.db.GenDirectory().GetNetworkEventCursor(ctx, key)
	if db.IsNotFound(err) {
		before, err = "", nil
	}
	if err != nil {
		return 0, err
	}
	page, err := read(ctx, iam.EventQuery{After: before, Limit: 100, Kinds: []iam.EventKind{iam.EventOAuthConsentRevoked, iam.EventUserAgreementAccepted}})
	if err != nil {
		return 0, fmt.Errorf("network: read account events: %w", err)
	}
	if len(page.Events) > 100 {
		return 0, fmt.Errorf("network: account event page exceeds limit")
	}
	if page.NextCursor == "" {
		return 0, fmt.Errorf("network: account events returned no continuation cursor")
	}
	if page.NextCursor == before {
		return 0, nil
	}
	count := 0
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.EnsureNetworkEventCursor(ctx, gen.EnsureNetworkEventCursorParams{Issuer: s.issuer, TenantID: uuid.UUID(s.tenant)}); err != nil {
			return err
		}
		current, err := q.LockNetworkEventCursor(ctx, gen.LockNetworkEventCursorParams{Issuer: s.issuer, TenantID: uuid.UUID(s.tenant)})
		if err != nil || current != before {
			return err
		}
		// All projection writes reuse this transaction, including nested
		// MerchantTx savepoints. AuthKit has its separate read pool.
		local := *s
		local.db = s.db.NewWithPgxTx(tx)
		for _, event := range page.Events {
			if event.Version != 1 {
				return fmt.Errorf("network: unsupported account event version %d", event.Version)
			}
			if err := local.applyEvent(ctx, event); err != nil {
				return err
			}
		}
		if err := q.SaveNetworkEventCursor(ctx, gen.SaveNetworkEventCursorParams{Issuer: s.issuer, TenantID: uuid.UUID(s.tenant), Cursor: page.NextCursor}); err != nil {
			return err
		}
		count = len(page.Events)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
