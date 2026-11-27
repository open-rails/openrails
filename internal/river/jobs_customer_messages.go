package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/billingfact"
	"github.com/open-rails/openrails/internal/modules/messages"
)

// CustomerMessageWorker settles one customer message, queued with its fact
// (billingfact.Record) and retried with River's backoff until its last
// attempt.
type CustomerMessageWorker struct {
	WorkerDefaults[billingfact.SendArgs]
	Messages *messages.Service
}

func (w *CustomerMessageWorker) Work(ctx context.Context, job *river.Job[billingfact.SendArgs]) error {
	if w.Messages == nil {
		return fmt.Errorf("customer message worker is not wired")
	}
	return w.Messages.Send(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}
