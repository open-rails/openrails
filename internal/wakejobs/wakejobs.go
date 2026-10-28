// Package wakejobs holds the wake-up jobs: a webhook or a fixed cause marks
// one provider object dirty, and a coalesced job reads it. A job is unique per
// object until it completes and debounced, so a burst collapses to one read; a
// wake landing on a running job chains a follow-up behind it, because that job
// may have read before the cause. The workers live in riverjobs.
package wakejobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/db"
)

const (
	KindSubscriptionConverge  = "openrails.subscription_converge"
	KindProviderObjectObserve = "openrails.provider_object_observe"

	// Queue is the billing queue both kinds run on.
	Queue = "billing"
	// Debounce delays the read so a burst of events about one object
	// coalesces into one provider read.
	Debounce = 5 * time.Second
	// maxChain bounds the follow-up chain. A follow-up waits for its
	// predecessor, so at most two jobs per object run at once.
	maxChain = 4
)

// SubscriptionConvergeArgs identifies one dirty subscription. EventType and
// EventCreated are forensics outside uniqueness.
type SubscriptionConvergeArgs struct {
	PSPID                 uuid.UUID `json:"psp_id" river:"unique"`
	MerchantID            uuid.UUID `json:"merchant_id" river:"unique"`
	Rail                  string    `json:"rail" river:"unique"`
	SubscriptionReference string    `json:"subscription_reference" river:"unique"`
	// After is the converge job already running when this wake arrived.
	After        int64  `json:"after,omitempty" river:"unique"`
	EventType    string `json:"event_type,omitempty"`
	EventCreated int64  `json:"event_created,omitempty"`
}

func (SubscriptionConvergeArgs) Kind() string { return KindSubscriptionConverge }

func (a SubscriptionConvergeArgs) follow(id int64) Args { a.After = id; return a }
func (a SubscriptionConvergeArgs) ref() string          { return a.SubscriptionReference }

// ProviderObjectObserveArgs identifies one provider catalog object to read.
type ProviderObjectObserveArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	PSPID      uuid.UUID `json:"psp_id" river:"unique"`
	// ObjectKind is product or price.
	ObjectKind string `json:"kind" river:"unique"`
	Ref        string `json:"ref" river:"unique"`
	After      int64  `json:"after,omitempty" river:"unique"`
	EventType  string `json:"event_type,omitempty"`
}

func (ProviderObjectObserveArgs) Kind() string { return KindProviderObjectObserve }

func (a ProviderObjectObserveArgs) follow(id int64) Args { a.After = id; return a }
func (a ProviderObjectObserveArgs) ref() string          { return a.ObjectKind + " " + a.Ref }

// Args are a wake job's arguments.
type Args interface {
	river.JobArgs
	follow(after int64) Args
	ref() string
}

// UniqueStates is River's default unique set minus completed, so a finished
// job never blocks the next wake. River requires running in the set.
var UniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// Inserter is where a wake is inserted.
type Inserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	// Retry makes a scheduled job available now.
	Retry(ctx context.Context, id int64) error
}

// Client inserts through the River client, each insert committing alone.
type Client struct{ River *river.Client[pgx.Tx] }

func (c Client) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if c.River == nil {
		return nil, fmt.Errorf("wake job: river client unavailable")
	}
	return c.River.Insert(ctx, args, opts)
}

func (c Client) Retry(ctx context.Context, id int64) error {
	_, err := c.River.JobRetry(ctx, id)
	return err
}

// Tx inserts in a transaction-scoped DB's transaction: the wake commits with
// the cause it follows.
type Tx struct{ DB *db.DB }

func (t Tx) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return t.DB.InsertRiverJob(ctx, args, opts)
}

func (t Tx) Retry(ctx context.Context, id int64) error { return t.DB.RetryRiverJob(ctx, id) }

// Enqueue wakes args' object after debounce (Debounce when zero). A job not
// running yet reads after this wake; one scheduled past the debounce (a
// snoozed one) is woken now; a running one gets a follow-up chained behind it.
func Enqueue(ctx context.Context, ins Inserter, args Args, debounce time.Duration) error {
	if debounce <= 0 {
		debounce = Debounce
	}
	opts := &river.InsertOpts{
		Queue:       Queue,
		ScheduledAt: time.Now().Add(debounce),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: UniqueStates},
	}
	for range maxChain {
		res, err := ins.Insert(ctx, args, opts)
		if err != nil {
			return err
		}
		if !res.UniqueSkippedAsDuplicate || res.Job == nil {
			return nil
		}
		if res.Job.State == rivertype.JobStateScheduled && res.Job.ScheduledAt.After(opts.ScheduledAt) {
			return ins.Retry(ctx, res.Job.ID)
		}
		if res.Job.State != rivertype.JobStateRunning {
			return nil
		}
		args = args.follow(res.Job.ID)
	}
	return fmt.Errorf("wake job %s: follow-up chain for %s exceeds %d running jobs", args.Kind(), args.ref(), maxChain)
}
