package books

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Recognize brings one of the merchant's due schedules up to now in its own
// transaction: each month boundary reached, dated the last instant before it,
// then at its end the rest, or at an early end the share its service earned
// and the remainder forfeited. A schedule another pass took is left alone, and
// a boundary posted before posts nothing again.
func Recognize(ctx context.Context, d *db.DB, id uuid.UUID, loc *time.Location, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := d.NewWithPgxTx(tx).Gen(ctx)
		row, err := q.LockDueRevenueSchedule(ctx, gen.LockDueRevenueScheduleParams{MerchantID: mid.UUID(), ID: id, Now: now})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		s := scheduleOf(row)
		steps, next, err := s.advance(ctx, loc, now)
		if err != nil {
			return err
		}
		if err := New(q, mid.UUID()).recognize(ctx, row, steps); err != nil {
			return err
		}
		n, err := q.SetRevenueScheduleRecognized(ctx, gen.SetRevenueScheduleRecognizedParams{
			MerchantID: mid.UUID(), ID: id, RecognizedNet: s.recognized, RecognizedThroughAt: s.through, NextRecognitionAt: next,
			Reinstatements: int32(s.reinstatements), // #nosec G115 -- one per undone end
		})
		if err == nil && n != 1 {
			err = fmt.Errorf("books: revenue schedule %s did not record its recognition", id)
		}
		return err
	})
}

// Replay books one order's money again through the live posters, for the
// books' rebuild: the receipt of each payment that brought it money and, once
// it is complete, their application to its lines. What posted live posts
// nothing again. A schedule the replay makes starts at its application; the
// recognition job then posts each boundary since, dated in its own month.
func Replay(ctx context.Context, d *db.DB, orderID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	receipts, err := q.ListOrderReceipts(ctx, gen.ListOrderReceiptsParams{MerchantID: mid.UUID(), OrderID: orderID})
	if err != nil {
		return err
	}
	for _, p := range receipts {
		if err := ReceivePayment(ctx, q, p); err != nil {
			return err
		}
	}
	return ApplyOrder(ctx, d, orderID, nil)
}
