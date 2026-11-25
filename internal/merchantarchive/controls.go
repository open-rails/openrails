package merchantarchive

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
)

// archiveBy names the archive as the writer of the controls it sets.
const archiveBy = "billing archive"

const exportReason = "billing export"

// fenceExport makes the source's writes readonly before its snapshot is
// taken. A provider write that passed the gate earlier is then either
// finished, and in the snapshot, or still in flight, and refused by the
// preflight. undo puts back the writes it found unless the controls changed
// since; a failed export calls it, a complete one leaves the source readonly.
func fenceExport(ctx context.Context, database *db.DB, id billing.MerchantID) (undo func(), err error) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	readonly := controls.ReadOnly
	var previous controls.Mode
	var fenced bool
	err = database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		q := gen.New(tx)
		if err := q.LockMerchantControls(ctx, id.UUID()); err != nil {
			return err
		}
		before, err := controls.Load(ctx, q, id.UUID())
		if err != nil {
			return err
		}
		previous = before.Writes
		_, fenced, err = controls.Set(ctx, q, id.UUID(), controls.Change{Writes: &readonly}, archiveBy, exportReason, at)
		return err
	})
	if err != nil || !fenced {
		return func() {}, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := scope(ctx, tx, id); err != nil {
				return err
			}
			q := gen.New(tx)
			if err := q.LockMerchantControls(ctx, id.UUID()); err != nil {
				return err
			}
			current, err := controls.Load(ctx, q, id.UUID())
			if err != nil || current.ChangedBy != archiveBy || current.Reason != exportReason || !current.ChangedAt.Equal(at) {
				return err
			}
			_, _, err = controls.Set(ctx, q, id.UUID(), controls.Change{Writes: &previous}, archiveBy, "billing export failed", time.Now())
			return err
		})
		if err != nil {
			database.Log.ErrorContext(ctx, "billing export failed and its source stays readonly; `openrails merchant writes full` restores writes", logx.Err(err), "merchant_id", id.String())
		}
	}, nil
}

// landReadonly makes a restored merchant readonly and disarmed until an
// operator arms it: its source may still be live.
func landReadonly(ctx context.Context, q *gen.Queries, id billing.MerchantID, at time.Time) error {
	readonly, disarmed := controls.ReadOnly, false
	_, _, err := controls.Set(ctx, q, id.UUID(), controls.Change{Writes: &readonly, DestructiveArmed: &disarmed}, archiveBy, "billing restore", at)
	return err
}
