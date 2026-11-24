package messages

import (
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// KindDispatch queues the send job of every pending message: the
	// backstop for a dispatcher that missed its wake.
	KindDispatch = "openrails.customer_messages_dispatch"
	// KindSend settles one message.
	KindSend = "openrails.customer_message"
	// DispatchInterval is the backstop's cadence.
	DispatchInterval = 5 * time.Minute
	// SendAttempts bounds a message's tries; the last failed one is failed.
	SendAttempts = 10

	queue = "billing"
)

// DispatchArgs is one backstop pass.
type DispatchArgs struct{}

func (DispatchArgs) Kind() string { return KindDispatch }

// InsertOpts runs one pass per period in one attempt: the next pass retries.
func (DispatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByPeriod: DispatchInterval}}
}

// SendArgs is one message's send job.
type SendArgs struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

func (SendArgs) Kind() string { return KindSend }

// InsertOpts makes one job per message, completed ones included.
func (SendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queue, MaxAttempts: SendAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateCompleted, rivertype.JobStatePending,
		rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}
