// Package billingfact records a billing fact, a notification_events row, with
// its customer's message and the message's send job in the caller's
// transaction: the job exists exactly when the fact commits. It sits below
// the modules that write facts and the messages package that sends them.
package billingfact

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

const (
	// SendKind settles one customer message.
	SendKind = "openrails.customer_message"
	// SendAttempts bounds a message's tries; the last failed one is failed.
	SendAttempts = 10
)

// SendArgs is one message's send job.
type SendArgs struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

func (SendArgs) Kind() string { return SendKind }

// InsertOpts makes one job per message, completed ones included.
func (SendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: SendAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateCompleted, rivertype.JobStatePending,
		rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}

// Fact is one billing fact about a customer.
type Fact struct {
	ID, MerchantID, CustomerID uuid.UUID
	EventType                  string
	// Data is the fact's JSON object; nil is {}.
	Data []byte
	// CreatedAt is when it happened; zero is the transaction's start.
	CreatedAt time.Time
}

// Record writes f once by its id in q's transaction, with its email message
// (queued) and that message's send job. It returns when f happened; zero,
// writing nothing, when f was recorded before. A billing restore's fact, or
// one whose message was logged before, queues no message. Without a River
// producer bound to q's transaction it is refused with db.ErrNoRiverProducer:
// a fact never commits without its message's job.
func Record(ctx context.Context, q *gen.Queries, f Fact) (time.Time, error) {
	row, err := q.RecordNotification(ctx, gen.RecordNotificationParams{
		ID: f.ID, MerchantID: f.MerchantID, CustomerID: f.CustomerID, EventType: f.EventType, Data: f.Data, CreatedAt: f.CreatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if row.MessageID != nil {
		if _, err := db.InsertRiverJobWith(ctx, q, SendArgs{MerchantID: f.MerchantID, MessageID: *row.MessageID}, nil); err != nil {
			return time.Time{}, fmt.Errorf("queue the customer message of %s: %w", f.EventType, err)
		}
	}
	return row.CreatedAt, nil
}
