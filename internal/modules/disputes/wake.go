package disputes

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/logx"
)

// wakeChannel is what billing.wake_dispute_responses notifies when what a
// response waits for changes; its payload is the schema.
const wakeChannel = "openrails_dispute_responses"

// Wake queues the response of every case still waiting, across merchants.
// Waiting cases are few and indexed: no case that finished is read.
func (s *Service) Wake(ctx context.Context) error {
	return s.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := gen.New(tx).ListWaitingDisputeResponses(ctx)
		if err != nil {
			return err
		}
		txDB := s.DB.NewWithPgxTx(tx)
		for _, row := range rows {
			if err := disputejob.QueueResponse(ctx, txDB, row.MerchantID, disputejob.Subject(row.Subject), row.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// wakeUntilDone wakes the waiting responses, retrying a failed wake with
// backoff: nothing waits for the next change, or a restart.
func (s *Service) wakeUntilDone(ctx context.Context) {
	for wait := time.Second; ctx.Err() == nil; wait = min(2*wait, time.Minute) {
		err := s.Wake(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		s.Log.WarnContext(ctx, "dispute responses: wake failed; retrying", logx.Err(err), "in", wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// Run wakes the waiting responses on every notification for this schema and
// on every (re)connect, for those queued while nothing listened, until ctx
// ends, on a session of its own.
func (s *Service) Run(ctx context.Context) {
	if s.DB.Pool() == nil {
		return
	}
	schema := s.DB.DataPool().Schema()
	s.DB.Listen(ctx, db.Listener{
		Channel:   wakeChannel,
		Connected: s.wakeUntilDone,
		Notified: func(ctx context.Context, payload string) {
			if payload == schema {
				s.wakeUntilDone(ctx)
			}
		},
		Interrupted: func(ctx context.Context, err error) {
			s.Log.WarnContext(ctx, "dispute responses: wake notifications interrupted; reconnecting", logx.Err(err))
		},
	})
}
