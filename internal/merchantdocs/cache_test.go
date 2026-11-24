package merchantdocs

import (
	"context"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"

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
