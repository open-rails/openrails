package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/modules/messages"
)

// CustomerMessageDispatchWorker queues the send job of every pending
// customer message, for wakes the dispatcher missed.
type CustomerMessageDispatchWorker struct {
	WorkerDefaults[messages.DispatchArgs]
	Messages *messages.Service
}

func (w *CustomerMessageDispatchWorker) Work(ctx context.Context, _ *river.Job[messages.DispatchArgs]) error {
	if w.Messages == nil {
		return fmt.Errorf("customer message dispatch worker is not wired")
	}
	return w.Messages.Drain(ctx)
}

// CustomerMessageWorker settles one customer message, retried with River's
// backoff until its last attempt.
type CustomerMessageWorker struct {
	WorkerDefaults[messages.SendArgs]
	Messages *messages.Service
}

func (w *CustomerMessageWorker) Work(ctx context.Context, job *river.Job[messages.SendArgs]) error {
	if w.Messages == nil {
		return fmt.Errorf("customer message worker is not wired")
	}
	return w.Messages.Send(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}
