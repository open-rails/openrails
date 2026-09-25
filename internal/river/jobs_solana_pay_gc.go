package riverjobs

import (
	"context"
	"errors"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
)

const KindSolanaPayGC = "openrails.solana_pay_gc"

const (
	solanaPayGCBatch   = 500
	solanaPayGCBatches = 20
)

type SolanaPayGCArgs struct{}

func (SolanaPayGCArgs) Kind() string { return KindSolanaPayGC }

// SolanaPayGCWorker deletes settled Solana Pay references whose watch window
// has passed, in bounded batches. Pending references and credited or review
// receipts are never touched.
type SolanaPayGCWorker struct {
	river.WorkerDefaults[SolanaPayGCArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (SolanaPayGCWorker) Kind() string { return KindSolanaPayGC }

func (w SolanaPayGCWorker) Work(ctx context.Context, _ *river.Job[SolanaPayGCArgs]) error {
	if w.Clock == nil {
		return errors.New("solana pay gc worker requires a clock")
	}
	now := w.Clock.Now()
	var total int64
	for range solanaPayGCBatches {
		n, err := solanamodule.DeleteSettled(ctx, w.DB, now, solanaPayGCBatch)
		if err != nil {
			return err
		}
		total += n
		if n < solanaPayGCBatch {
			break
		}
	}
	if total > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"worker": KindSolanaPayGC, "deleted": total}).Info("deleted settled Solana Pay references")
	}
	return nil
}
