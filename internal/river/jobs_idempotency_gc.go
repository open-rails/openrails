package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/idempotency"
)

const KindIdempotencyGC = "openrails.idempotency_gc"

const (
	// idempotencyGCBatch bounds one delete statement and so one transaction.
	idempotencyGCBatch = 1000
	// idempotencyGCMaxBatches bounds one pass; a larger backlog drains over
	// the following passes.
	idempotencyGCMaxBatches = 200
)

type IdempotencyGCArgs struct{}

func (IdempotencyGCArgs) Kind() string { return KindIdempotencyGC }

// IdempotencyGCWorker deletes expired request and webhook claims (#1099).
type IdempotencyGCWorker struct {
	river.WorkerDefaults[IdempotencyGCArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (IdempotencyGCWorker) Kind() string { return KindIdempotencyGC }

func (w IdempotencyGCWorker) Work(ctx context.Context, _ *river.Job[IdempotencyGCArgs]) error {
	_, err := w.Sweep(ctx)
	return err
}

// Sweep deletes expired rows in batches until a batch comes back short, and
// returns how many it deleted.
func (w IdempotencyGCWorker) Sweep(ctx context.Context) (int64, error) {
	if w.DB == nil || w.Clock == nil {
		return 0, fmt.Errorf("idempotency gc requires a database and a clock")
	}
	now := w.Clock.Now()
	var total int64
	for range idempotencyGCMaxBatches {
		n, err := idempotency.DeleteExpired(ctx, w.DB, now, idempotencyGCBatch)
		if err != nil {
			return total, fmt.Errorf("delete expired idempotency keys: %w", err)
		}
		total += n
		if n < idempotencyGCBatch {
			break
		}
	}
	if total > 0 {
		log.WithContext(ctx).WithField("worker", KindIdempotencyGC).WithField("deleted", total).Info("deleted expired idempotency keys")
	}
	return total, nil
}
