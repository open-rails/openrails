package riverjobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/intents/wake"
)

const (
	// KindOperationWakes names the scan in the billing pass.
	KindOperationWakes = "openrails.operation_wakes"
	// OperationWakeGrace is how long past due a live operation waits before
	// its wake counts as lost: River runs a due wake in seconds.
	OperationWakeGrace = time.Hour
	operationWakeBatch = 500
)

// OperationWakes queues again the wake of each live provider operation more
// than OperationWakeGrace past due: its River job was deleted, its schema
// reset or its fleet stopped. Every write that leaves an operation live queues
// its wake, so the scan finds none unless one was lost or the operation is
// held past due. A duplicate wake is harmless: claims are canonical.
type OperationWakes struct {
	DB    *db.DB
	Clock clockwork.Clock
	Log   *slog.Logger
}

func (w *OperationWakes) Pass(ctx context.Context) error {
	before := workerNow(w.Clock).Add(-OperationWakeGrace)
	var afterAt time.Time
	var afterID uuid.UUID
	for {
		overdue, err := w.DB.GenDirectory().ListOverdueProviderOperations(ctx, gen.ListOverdueProviderOperationsParams{
			Before: before, AfterAt: afterAt, AfterID: afterID, RequestOnlyTypes: intents.ManualPaymentMethodDeleteTypes(), RowLimit: operationWakeBatch,
		})
		if err != nil {
			return fmt.Errorf("operation wakes: list overdue: %w", err)
		}
		var queued []gen.ListOverdueProviderOperationsRow
		if len(overdue) > 0 {
			err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				for _, op := range overdue {
					inserted, err := wake.Requeue(ctx, w.DB, tx, op.MerchantID, op.ID)
					if err != nil {
						return fmt.Errorf("operation %s: %w", op.ID, err)
					}
					if inserted {
						queued = append(queued, op)
					}
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("operation wakes: %w", err)
			}
			last := overdue[len(overdue)-1]
			afterAt, afterID = last.NextAttemptAt, last.ID
		}
		for _, op := range queued {
			w.Log.WarnContext(ctx, "provider operation an hour past due; its wake is queued again", "merchant_id", op.MerchantID, "intent_id", op.ID, "due_at", op.NextAttemptAt)
		}
		if len(overdue) < operationWakeBatch {
			return nil
		}
	}
}
