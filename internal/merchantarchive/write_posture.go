package merchantarchive

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/writeposture"
)

// postureSetBy names the archive as the writer of a posture it sets.
const postureSetBy = "billing archive"

// fenceExport makes the source readonly before its snapshot is taken. A
// provider write that passed the gate earlier is then either finished, and in
// the snapshot, or still in flight, and refused by the preflight. undo puts
// back the posture it found unless something changed it since; a failed
// export calls it, a complete one leaves the source readonly.
func fenceExport(ctx context.Context, database *db.DB, id billing.MerchantID) (undo func(), err error) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	var previous *gen.LockWritePostureRow
	err = database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		q := gen.New(tx)
		row, err := q.LockWritePosture(ctx, id.UUID())
		switch {
		case err == nil:
			previous = &row
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return writeposture.Set(ctx, q, id.UUID(), writeposture.ReadOnly, writeposture.ReasonExported, postureSetBy, at)
	})
	if err != nil {
		return nil, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := scope(ctx, tx, id); err != nil {
				return err
			}
			q := gen.New(tx)
			row, err := q.LockWritePosture(ctx, id.UUID())
			if err != nil || row.Reason != writeposture.ReasonExported || row.SetBy != postureSetBy || !row.SetAt.Equal(at) {
				return err
			}
			if previous == nil {
				return q.DeleteWritePosture(ctx, id.UUID())
			}
			return q.SetWritePosture(ctx, gen.SetWritePostureParams{MerchantID: id.UUID(), Mode: previous.Mode, Reason: previous.Reason, SetBy: previous.SetBy, SetAt: previous.SetAt})
		})
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", id.String()).
				Error("billing export failed and its source stays readonly; `openrails merchant arm` restores writes")
		}
	}, nil
}

// landReadonly makes a restored merchant readonly until an operator arms it:
// its source may still be live.
func landReadonly(ctx context.Context, q *gen.Queries, id billing.MerchantID, at time.Time) error {
	return writeposture.Set(ctx, q, id.UUID(), writeposture.ReadOnly, writeposture.ReasonRestored, postureSetBy, at)
}
