package messages

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
)

// channel is what billing.queue_customer_message notifies; its payload is
// the schema that queued.
const channel = "openrails_customer_messages"

// dispatchBatch bounds one claim and its transaction.
const dispatchBatch = 100

// Drain queues the send job of every pending message, a batch per
// transaction: a claimed message and its job commit together.
func (s *Service) Drain(ctx context.Context) error {
	for {
		claimed := 0
		err := s.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := gen.New(tx).ClaimPendingCustomerMessages(ctx, gen.ClaimPendingCustomerMessagesParams{Now: s.Clock.Now().UTC(), Batch: dispatchBatch})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if _, err := s.DB.InsertRiverJobTx(ctx, tx, SendArgs{MerchantID: row.MerchantID, MessageID: row.ID}, nil); err != nil {
					return err
				}
			}
			claimed = len(rows)
			return nil
		})
		if err != nil || claimed < dispatchBatch {
			return err
		}
	}
}

// Run drains on every wake until ctx ends: each commit that queued a
// message, and every (re)connect, for those queued while nothing listened.
func (s *Service) Run(ctx context.Context) {
	if s.DB.Pool() == nil {
		return
	}
	wake := make(chan struct{}, 1)
	listening := make(chan struct{})
	go func() { defer close(listening); s.listen(ctx, wake) }()
	defer func() { <-listening }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		if err := s.Drain(ctx); err != nil && ctx.Err() == nil {
			s.Log.WarnContext(ctx, "customer messages: dispatch failed; the periodic pass queues them", logx.Err(err))
		}
	}
}

// listen rings wake for this schema's notifications, on a dedicated session.
func (s *Service) listen(ctx context.Context, wake chan<- struct{}) {
	schema := s.DB.DataPool().Schema()
	ring := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	for ctx.Err() == nil {
		err := func() error {
			conn, err := s.DB.ConnectSession(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
				return err
			}
			ring()
			for {
				n, err := conn.WaitForNotification(ctx)
				if err != nil {
					return err
				}
				if n.Payload == schema {
					ring()
				}
			}
		}()
		if err != nil && ctx.Err() == nil {
			s.Log.WarnContext(ctx, "customer messages: wake notifications interrupted; reconnecting", logx.Err(err))
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}
