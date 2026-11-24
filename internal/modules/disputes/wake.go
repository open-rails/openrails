package disputes

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

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

// Run wakes the waiting responses on every notification for this schema and
// on every (re)connect, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if s.DB.Pool() == nil {
		return
	}
	schema := s.DB.DataPool().Schema()
	for ctx.Err() == nil {
		err := func() error {
			conn, err := s.DB.ConnectSession(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{wakeChannel}.Sanitize()); err != nil {
				return err
			}
			for {
				if err := s.Wake(ctx); err != nil && ctx.Err() == nil {
					s.Log.WarnContext(ctx, "dispute responses: wake failed; the next change wakes them", logx.Err(err))
				}
				for {
					n, err := conn.WaitForNotification(ctx)
					if err != nil {
						return err
					}
					if n.Payload == schema {
						break
					}
				}
			}
		}()
		if err != nil && ctx.Err() == nil {
			s.Log.WarnContext(ctx, "dispute responses: wake notifications interrupted; reconnecting", logx.Err(err))
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}
