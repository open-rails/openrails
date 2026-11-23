package app

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/logx"
	"runtime/debug"
	"sync"
)

type backgroundTasks struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
}

// Go runs fn on a goroutine owned by the runtime until Close. A panic is
// logged and ends only that task.
func (r *Runtime) Go(name string, fn func(context.Context)) {
	b := &r.background
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithCancel(context.Background())
	}
	ctx := b.ctx
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				r.Log.ErrorContext(ctx, "background task panicked", "task", name, "stack", string(debug.Stack()), logx.Err(fmt.Errorf("panic: %v", p)))
			}
		}()
		fn(ctx)
	}()
}

func (b *backgroundTasks) stop() {
	b.mu.Lock()
	b.stopped = true
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.wg.Wait()
}
