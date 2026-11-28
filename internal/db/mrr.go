package db

import (
	"context"

	"github.com/open-rails/openrails/internal/db/gen"
)

// MRRCauseGUC names, for the rest of a transaction, what moves MRR when its
// triggers cannot tell from the rows: an undo restoring subscriptions
// (corrections). An import writes its history itself
// (billing.record_mrr_history).
const MRRCauseGUC = "openrails.mrr_cause"

// MRRCauseUndo is the cause MRRCauseGUC may name.
const MRRCauseUndo = "undo"

// SetMRRCause names cause for the MRR the caller's transaction moves.
func SetMRRCause(ctx context.Context, q *gen.Queries, cause string) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: MRRCauseGUC, Value: cause, IsLocal: true})
	return err
}
