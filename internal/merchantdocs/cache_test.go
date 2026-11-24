package merchantdocs

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/logx"
)

type countingSource struct {
	*FileSource
	loads atomic.Int64
}

func (s *countingSource) Load(ctx context.Context, id billing.MerchantID) (Set, error) {
	s.loads.Add(1)
	return s.FileSource.Load(ctx, id)
}

// Close returns with no reload running, so a walk of the cache after it
// races nothing while edits are still being announced.
func TestCloseWaitsForReloads(t *testing.T) {
	for range 20 {
		source := &countingSource{FileSource: NewFileSource()}
		id := billing.MerchantID(uuid.New())
		source.Put(id, Set{PSPs: map[string]Doc[PSP]{"card": {}}})
		c := NewCache(slog.New(slog.DiscardHandler), source)
		if _, err := c.Get(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
					c.Invalidate(id)
				}
			}
		}()
		for source.loads.Load() < 10 {
			runtime.Gosched()
		}
		c.Close()
		if unset := logx.Unset(c); len(unset) > 0 {
			t.Fatalf("Unset = %v", unset)
		}
		close(stop)
		<-done
	}
}

// A load is the process's: neither the caller's context values (its pinned
// connection, its transaction) nor its end reach it, and concurrent callers
// share one load.
func TestLoadRunsOnTheCachesOwnContext(t *testing.T) {
	source := &countingSource{FileSource: NewFileSource()}
	id := billing.MerchantID(uuid.New())
	source.Put(id, Set{})
	c := NewCache(slog.New(slog.DiscardHandler), source)
	defer c.Close()
	type callerKey struct{}
	release := make(chan struct{})
	var syncs atomic.Int64
	leaked := make(chan string, 2)
	c.SetSync(func(ctx context.Context, _ billing.MerchantID, _, loaded Set) (Set, error) {
		syncs.Add(1)
		if ctx.Value(callerKey{}) != nil {
			leaked <- "the caller's context values"
		}
		<-release
		if ctx.Err() != nil {
			leaked <- "the caller's end"
		}
		return loaded, nil
	})
	caller, cancel := context.WithCancel(context.WithValue(t.Context(), callerKey{}, true))
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := c.Get(caller, id)
			errs <- err
		}()
	}
	for syncs.Load() == 0 {
		runtime.Gosched()
	}
	cancel()
	for range 8 {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("a caller that gave up got %v", err)
		}
	}
	close(release)
	if _, err := c.Get(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if n := syncs.Load(); n != 1 {
		t.Fatalf("%d loads ran; the callers share one", n)
	}
	select {
	case what := <-leaked:
		t.Fatalf("%s reached the load", what)
	default:
	}
}

// A first load that failed answers its error until loadRetry has passed, so
// a failure every caller would meet is not run again for each of them.
func TestFailedLoadIsNotRunAgainForEveryCaller(t *testing.T) {
	source := &countingSource{FileSource: NewFileSource()}
	id := billing.MerchantID(uuid.New())
	c := NewCache(slog.New(slog.DiscardHandler), source)
	defer c.Close()
	now := time.Now()
	c.now = func() time.Time { return now }
	broken := errors.New("database out of reach")
	var syncs atomic.Int64
	c.SetSync(func(context.Context, billing.MerchantID, Set, Set) (Set, error) {
		syncs.Add(1)
		return Set{}, broken
	})
	for range 5 {
		if _, err := c.Get(t.Context(), id); !errors.Is(err, broken) {
			t.Fatalf("got %v, want the load's failure", err)
		}
	}
	if n := syncs.Load(); n != 1 {
		t.Fatalf("%d loads ran within loadRetry", n)
	}
	now = now.Add(loadRetry)
	_, _ = c.Get(t.Context(), id)
	if n := syncs.Load(); n != 2 {
		t.Fatalf("%d loads ran; want a second after loadRetry", n)
	}
}
