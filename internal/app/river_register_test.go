package app

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/alerting"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

type loggedWorker struct {
	riverjobs.WorkerDefaults[alerting.DeliveryArgs]
	Log *slog.Logger
}

func (loggedWorker) Work(context.Context, *river.Job[alerting.DeliveryArgs]) error { return nil }

// A walk of the runtime reaches every worker addWorker registered.
func TestUnsetReachesRegisteredWorkers(t *testing.T) {
	workers := &workerSet{river: river.NewWorkers()}
	require.NoError(t, addWorker(workers, &loggedWorker{}))
	r := &Runtime{Log: slog.Default(), riverWorkers: workers.added}
	require.True(t, slices.Contains(logx.Unset(r), "*app.Runtime.riverWorkers[].Log"), logx.Unset(r))
}

type configWorker struct {
	riverjobs.WorkerDefaults[alerting.DeliveryArgs]
	Config *merchantdocs.Cache
	Log    *slog.Logger
}

func (configWorker) Work(context.Context, *river.Job[alerting.DeliveryArgs]) error { return nil }

type countingSource struct {
	*merchantdocs.FileSource
	loads atomic.Int64
}

func (s *countingSource) Load(ctx context.Context, id billing.MerchantID) (merchantdocs.Set, error) {
	s.loads.Add(1)
	return s.FileSource.Load(ctx, id)
}

// addWorker registers a worker whose graph the runtime is serving, as
// provider posture loads merchants and announced edits reload them, and
// reads none of that state.
func TestAddWorkerWhileMerchantConfigLoads(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for name, serve := range map[string]func(context.Context, *merchantdocs.Cache, billing.MerchantID){
		"posture loads a merchant": func(ctx context.Context, c *merchantdocs.Cache, _ billing.MerchantID) {
			_, _ = c.Get(ctx, billing.MerchantID(uuid.New()))
		},
		"an announced edit reloads one": func(_ context.Context, c *merchantdocs.Cache, id billing.MerchantID) {
			c.Invalidate(id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := &countingSource{FileSource: merchantdocs.NewFileSource()}
			id := billing.MerchantID(uuid.New())
			source.Put(id, merchantdocs.Set{PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{"card": {}}, Rejected: map[string]string{}})
			cache := merchantdocs.NewCache(logger, source)
			t.Cleanup(cache.Close)
			_, err := cache.Get(t.Context(), id)
			require.NoError(t, err)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for source.loads.Load() < 200 {
					serve(t.Context(), cache, id)
				}
			}()
			for running := true; running; {
				select {
				case <-done:
					running = false
				default:
				}
				require.NoError(t, addWorker(&workerSet{river: river.NewWorkers()}, &configWorker{Config: cache, Log: logger}))
			}
		})
	}
}
