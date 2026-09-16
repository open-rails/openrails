package riverjobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// loadSweepCursor reads a kind's fair-sweep ring position together with the
// opaque version the pass must present when it saves. A kind that has never
// swept starts at the beginning of the ring with version zero.
func loadSweepCursor(ctx context.Context, q *gen.Queries, kind string) (gen.GetSweepCursorRow, error) {
	row, err := q.GetSweepCursor(ctx, kind)
	if err != nil && !db.IsNotFound(err) {
		return gen.GetSweepCursorRow{}, fmt.Errorf("load sweep cursor: %w", err)
	}
	return row, nil
}

// saveSweepCursor persists the ring position for the next pass. The save is a
// compare-and-swap on the version this pass read: a pass that finishes after a
// newer pass already moved the cursor keeps the newer position instead of
// moving it back. Either outcome costs fairness on the next pass, not
// correctness, so neither is a job error.
func saveSweepCursor(ctx context.Context, q *gen.Queries, kind string, read gen.GetSweepCursorRow, next *uuid.UUID, logger *log.Entry) {
	n, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{
		WorkerKind:            kind,
		CursorMerchantID:      next,
		ExpectedCursorVersion: read.CursorVersion,
	})
	if err != nil {
		logger.WithError(err).Warn("sweep: could not persist cursor")
		return
	}
	if n == 0 {
		logger.Info("sweep: cursor already moved by a newer pass; keeping it")
	}
}
