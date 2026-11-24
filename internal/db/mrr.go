package db

import (
	"context"

	"github.com/open-rails/openrails/internal/db/gen"
)

// MRRCauseGUC names, for the rest of a transaction, what moves MRR when its
// triggers cannot tell from the rows: an import adopting subscriptions
// (opening movements) or an undo restoring them (corrections).
const MRRCauseGUC = "openrails.mrr_cause"

// The causes MRRCauseGUC may name.
const (
	MRRCauseImport = "import"
	MRRCauseUndo   = "undo"
)

// SetMRRCause names cause for the MRR the caller's transaction moves.
func SetMRRCause(ctx context.Context, q *gen.Queries, cause string) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: MRRCauseGUC, Value: cause, IsLocal: true})
	return err
}
