package app

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	log "github.com/sirupsen/logrus"
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
				log.WithField("task", name).WithField("stack", string(debug.Stack())).Error(fmt.Sprintf("background task panicked: %v", p))
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

// dependencyState is a cached up/down observation of an optional provider.
type dependencyState struct {
	mu  sync.Mutex
	set bool
	err error
}

func (d *dependencyState) record(err error) {
	d.mu.Lock()
	d.set, d.err = true, err
	d.mu.Unlock()
}

// observed returns whether there has been an observation, and the last one.
func (d *dependencyState) observed() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.set, d.err
}

// ReportSignerKeyChange records that a Vault Transit signer key no longer
// matches its stored Solana identity (nil once an operator approves it).
func (r *Runtime) ReportSignerKeyChange(err error) {
	r.signerIdentity.record(err)
}

// SignerIdentityState is nil unless a Transit signer key change awaits approval.
func (r *Runtime) SignerIdentityState() error {
	_, err := r.signerIdentity.observed()
	return err
}
