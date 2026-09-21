//go:build integration

package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

type shutdownArgs struct{}

func (shutdownArgs) Kind() string { return "test_shutdown_dependency_order" }

type shutdownWorker struct {
	river.WorkerDefaults[shutdownArgs]
	pool     *pgxpool.Pool
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	cleanup  chan error
}

func (w *shutdownWorker) Work(ctx context.Context, _ *river.Job[shutdownArgs]) error {
	close(w.started)
	<-ctx.Done()
	close(w.canceled)
	<-w.release
	// A canceled worker can still need the pool to finish its terminal work.
	w.cleanup <- w.pool.Ping(context.Background())
	return ctx.Err()
}

func TestRuntimeCloseJoinsCanceledRiverBeforeClosingPools(t *testing.T) {
	appDB, dsn := testRuntimeDB(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	worker := &shutdownWorker{pool: pool, started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), cleanup: make(chan error, 1)}
	release := sync.OnceFunc(func() { close(worker.release) })
	defer release()
	registry := river.NewWorkers()
	river.AddWorker(registry, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "public", Workers: registry, Queues: map[string]river.QueueConfig{"shutdown_test": {MaxWorkers: 1}}})
	require.NoError(t, err)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() { _ = client.StopAndCancel(context.Background()) })
	_, err = client.Insert(t.Context(), shutdownArgs{}, &river.InsertOpts{Queue: "shutdown_test", MaxAttempts: 1})
	require.NoError(t, err)
	select {
	case <-worker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start")
	}
	rt := &Runtime{DB: appDB, RiverClient: client, riverPool: pool, riverStarted: true}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- rt.Close(canceledCtx) }()
	select {
	case <-worker.canceled:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("Close did not cancel in-flight worker after Stop failed")
	}
	require.NoError(t, pool.Ping(t.Context()), "pool must remain available until worker shutdown joins")
	select {
	case err := <-closed:
		release()
		t.Fatalf("Close returned before worker finished: %v", err)
	default:
	}
	release()
	require.NoError(t, <-worker.cleanup)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not join worker shutdown")
	}
}
