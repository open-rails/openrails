package riverjobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Only armed fetchers run — the allowed map gates lane readiness (solana
// included since #714/#715), and per-merchant arming (#699) decides the rest.
func TestProviderRefreshProvidersFollowArmedFetchers(t *testing.T) {
	fetchers := map[reconcile.Provider]reconcile.RailFetcher{
		reconcile.ProviderSolana: providerRefreshFakeFetcher{provider: reconcile.ProviderSolana},
		reconcile.ProviderStripe: providerRefreshFakeFetcher{provider: reconcile.ProviderStripe},
		reconcile.ProviderNMI:    providerRefreshFakeFetcher{provider: reconcile.ProviderNMI},
		reconcile.ProviderCCBill: providerRefreshFakeFetcher{provider: reconcile.ProviderCCBill},
	}

	require.Equal(t, []reconcile.Provider{
		reconcile.ProviderCCBill,
		reconcile.ProviderNMI,
		reconcile.ProviderSolana,
		reconcile.ProviderStripe,
	}, refreshProviders(fetchers))

	// No fetcher armed (merchant has no accounts on any rail) => no pulls.
	require.Empty(t, refreshProviders(nil))
}

type fakeRefreshInsert struct {
	args ProviderRefreshMerchantArgs
	opts *river.InsertOpts
}

type fakeRefreshInserter struct {
	inserts []fakeRefreshInsert
	dup     map[uuid.UUID]bool  // pretend an in-flight job already exists
	errFor  map[uuid.UUID]error // inject insert failures
}

func (f *fakeRefreshInserter) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	a, ok := args.(ProviderRefreshMerchantArgs)
	if !ok {
		return nil, fmt.Errorf("unexpected args type %T", args)
	}
	if err := f.errFor[a.MerchantID]; err != nil {
		return nil, err
	}
	f.inserts = append(f.inserts, fakeRefreshInsert{args: a, opts: opts})
	return &rivertype.JobInsertResult{UniqueSkippedAsDuplicate: f.dup[a.MerchantID]}, nil
}

func (f *fakeRefreshInserter) merchantSet() map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, ins := range f.inserts {
		out[ins.args.MerchantID] = true
	}
	return out
}

// #719 fan-out: one job per active merchant, minus the zero-accounts skip (no
// boot plane), each with the per-merchant in-flight unique key, spread evenly
// over Stagger on the default bounded queue.
func TestProviderRefreshSchedulerFanOut(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ins := &fakeRefreshInserter{}
	w := &ProviderRefreshSchedulerWorker{
		Clock:    clockwork.NewFakeClockAt(now),
		Stagger:  30 * time.Minute,
		Inserter: ins,
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			return []uuid.UUID{a, b, c}, nil
		},
		// c has zero declared rail accounts and there is no boot plane → skipped.
		HasRailAccounts: func(_ context.Context, mid uuid.UUID) (bool, error) {
			return mid != c, nil
		},
	}
	require.NoError(t, w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))

	require.Equal(t, map[uuid.UUID]bool{a: true, b: true}, ins.merchantSet())
	offsets := map[time.Duration]bool{}
	for _, got := range ins.inserts {
		require.Equal(t, QueueProviderRefresh, got.opts.Queue)
		require.True(t, got.opts.UniqueOpts.ByArgs, "unique per merchant_id")
		require.Equal(t, providerRefreshUniqueStates, got.opts.UniqueOpts.ByState)
		offsets[got.opts.ScheduledAt.Sub(now)] = true
	}
	// Slots are 0..k evenly spaced by Stagger/len(merchants); the skipped
	// merchant consumes no slot. Slot 0 fires immediately.
	require.Equal(t, map[time.Duration]bool{0: true, 10 * time.Minute: true}, offsets)
}

// Any boot-config fallback plane can arm every merchant, so the accounts
// predicate must not run (and nobody is skipped).
func TestProviderRefreshSchedulerBootPlaneSkipsPredicate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ins := &fakeRefreshInserter{}
	w := &ProviderRefreshSchedulerWorker{
		CCBillDataLink: &ccbill.DataLinkClient{},
		Inserter:       ins,
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			return []uuid.UUID{a, b}, nil
		},
		HasRailAccounts: func(context.Context, uuid.UUID) (bool, error) {
			t.Fatal("accounts predicate must not run when a boot plane exists")
			return false, nil
		},
	}
	require.NoError(t, w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))
	require.Equal(t, map[uuid.UUID]bool{a: true, b: true}, ins.merchantSet())
}

// Embedded hosts route the kind onto their existing billing queue.
func TestProviderRefreshSchedulerMerchantQueueOverride(t *testing.T) {
	ins := &fakeRefreshInserter{}
	w := &ProviderRefreshSchedulerWorker{
		MerchantQueue: QueueBilling,
		Inserter:      ins,
		Rails:         config.RailMerchantAccountSet{"nmi": nil},
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			return []uuid.UUID{uuid.New()}, nil
		},
	}
	require.NoError(t, w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))
	require.Len(t, ins.inserts, 1)
	require.Equal(t, QueueBilling, ins.inserts[0].opts.Queue)
	// Single merchant fires immediately (embedded boot must not wait out jitter).
	require.WithinDuration(t, time.Now(), ins.inserts[0].opts.ScheduledAt, time.Minute)
}

// Readonly mode: pure observer — no fan-out at all.
func TestProviderRefreshSchedulerReadonlySkips(t *testing.T) {
	ins := &fakeRefreshInserter{}
	w := &ProviderRefreshSchedulerWorker{
		Config:   &config.Config{Env: "dev", ProviderWriteMode: config.ProviderWriteModeReadOnly},
		Inserter: ins,
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			t.Fatal("readonly mode must not list merchants")
			return nil, nil
		},
	}
	require.NoError(t, w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))
	require.Empty(t, ins.inserts)
}

// Duplicate unique inserts are a clean no-op; hard insert failures fail the
// scheduler job so river retries it (reruns are idempotent via unique keys).
func TestProviderRefreshSchedulerDedupeAndErrors(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ins := &fakeRefreshInserter{dup: map[uuid.UUID]bool{a: true}}
	w := &ProviderRefreshSchedulerWorker{
		Rails:    config.RailMerchantAccountSet{"nmi": nil},
		Inserter: ins,
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			return []uuid.UUID{a, b}, nil
		},
	}
	require.NoError(t, w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))
	require.Equal(t, map[uuid.UUID]bool{a: true, b: true}, ins.merchantSet())

	ins.errFor = map[uuid.UUID]error{b: fmt.Errorf("boom")}
	err := w.Work(context.Background(), &river.Job[ProviderRefreshArgs]{})
	require.ErrorContains(t, err, "enqueues failed")
}

type providerRefreshFakeFetcher struct {
	provider reconcile.Provider
}

func (f providerRefreshFakeFetcher) Name() string { return string(f.provider) }

func (f providerRefreshFakeFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true, Vault: true}
}

func (f providerRefreshFakeFetcher) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return &reconcile.RemoteSnapshot{
		Provider:     f.provider,
		Capabilities: f.Capabilities(),
		Coverage: reconcile.SnapshotCoverage{
			SubscriptionsExhaustive:       true,
			TransactionsExhaustive:        true,
			TransactionsPaginatedComplete: true,
		},
	}, nil
}
