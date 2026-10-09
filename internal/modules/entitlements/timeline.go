package entitlements

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Timeline helpers: a customer's windows of one product form its timeline.
// All take an open transaction (gen.DBTX) so they run inside the caller's
// MerchantTx with the timeline lock held.

func accessTimelineLockKey(userID string, product uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write(product[:])
	key, _ := safecast.Convert[int64](h.Sum64() & math.MaxInt64)
	return key
}

// LockAccessTimeline serializes timeline writes per (customer, product),
// whatever their source. qx MUST be an open transaction: the advisory lock is
// transaction-scoped. The customer decision mutex is taken first.
func LockAccessTimeline(ctx context.Context, qx gen.DBTX, userID string, product uuid.UUID) error {
	if userID == "" || product == uuid.Nil {
		return fmt.Errorf("userID and product are required for an access timeline lock")
	}
	id, err := db.ResolveCustomerID(userID)
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := gen.New(qx)
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: id}); err != nil {
		return err
	}
	return q.AcquireAccessTimelineLock(ctx, accessTimelineLockKey(userID, product))
}

// ShiftAccessTimeline moves the customer's windows of the product that start
// at or after from by delta, except excludeIDs.
func ShiftAccessTimeline(ctx context.Context, qx gen.DBTX, customer, product uuid.UUID, from time.Time, delta time.Duration, now time.Time, excludeIDs []uuid.UUID) error {
	deltaSeconds := int64(delta.Seconds())
	if deltaSeconds == 0 {
		return nil
	}
	if excludeIDs == nil {
		excludeIDs = []uuid.UUID{}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return gen.New(qx).ShiftAccessTimelineWindows(ctx, gen.ShiftAccessTimelineWindowsParams{
		MerchantID: mid.UUID(), CustomerID: customer, ProductID: product,
		DeltaSeconds: deltaSeconds, Now: now, FromAt: from, ExcludeIds: excludeIDs,
	})
}

// GetAccessTimelineTailEnd returns the latest finite end of the customer's
// live windows of the product, or nil when it has none.
func GetAccessTimelineTailEnd(ctx context.Context, qx gen.DBTX, customer, product uuid.UUID) (*time.Time, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	end, err := gen.New(qx).GetAccessTimelineTailEnd(ctx, gen.GetAccessTimelineTailEndParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return end, nil
}
