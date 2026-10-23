package eventlog

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Prune removes a bounded prefix of expired facts. It owns no reader state;
// the recorded floor lets readers distinguish expiry from an empty new page.
func Prune(ctx context.Context, database *db.DB, now time.Time, retention time.Duration, limit int) (int, error) {
	if limit < 1 || limit > math.MaxInt32 || retention <= 0 {
		return 0, fmt.Errorf("event log pruning needs a bounded positive limit and retention")
	}
	cutoff := now.Add(-retention)
	merchants, err := database.GenDirectory().ListExpiredEventLogMerchants(ctx, gen.ListExpiredEventLogMerchantsParams{Cutoff: cutoff, RowLimit: int32(limit)})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, scopedID := range merchants {
		if scopedID == nil {
			return removed, fmt.Errorf("event log has a missing merchant scope")
		}
		mid := *scopedID
		if removed >= limit {
			break
		}
		count := 0
		err := database.DataPool().MerchantTx(ctx, billing.MerchantID(mid), func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			if err := q.LockEventLogStream(ctx, mid); err != nil {
				return err
			}
			remaining, err := safecast.Convert[int32](limit - removed)
			if err != nil {
				return err
			}
			gone, err := q.PruneEventLog(ctx, gen.PruneEventLogParams{MerchantID: mid, Cutoff: cutoff, RowLimit: remaining})
			if err != nil {
				return err
			}
			if gone.Removed > 0 {
				if err := q.AdvanceEventLogFloor(ctx, gen.AdvanceEventLogFloorParams{MerchantID: mid, Position: gone.Position, ID: gone.ID}); err != nil {
					return err
				}
			}
			count = int(gone.Removed)
			return nil
		})
		if err != nil {
			return removed, err
		}
		removed += count
	}
	return removed, nil
}
