// Package convergejob is a customer's convergence job, committed in the
// transaction of the mutation that changed the customer. It sits below the
// modules so their writers can queue it.
package convergejob

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
)

const Kind = "openrails.customer_converge"

// Args names one customer. After is the job already running when the
// mutation committed: it may have read before it, so this one reads again.
type Args struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	CustomerID uuid.UUID `json:"customer_id" river:"unique"`
	After      int64     `json:"after,omitempty" river:"unique"`
}

func (Args) Kind() string { return Kind }

// uniqueStates leave out completed, so a finished job never absorbs the next
// mutation. River requires running in the set; a mutation that lands on a
// running job queues a follow-up instead.
var uniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
	rivertype.JobStateRunning, rivertype.JobStateScheduled,
}

// InsertOpts gives convergence River's lowest priority: a bulk import or
// pull queues one per customer, and charges, renewals and provider reads
// waiting in the queue run first.
func (Args) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", Priority: 4, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: uniqueStates}}
}

// maxChain bounds the follow-ups behind running jobs.
const maxChain = 4

// Queue commits the customer's convergence with d's transaction. A handle
// outside a transaction queues nothing: its writer has no commit to join. A
// process with no River producer (an operator tool) queues nothing either;
// the operator's converge command covers it.
func Queue(ctx context.Context, d *db.DB, merchant billing.MerchantID, customer uuid.UUID) error {
	tx := d.Tx()
	if tx == nil || merchant.IsZero() || customer == uuid.Nil {
		return nil
	}
	args := Args{MerchantID: merchant.UUID(), CustomerID: customer}
	opts := args.InsertOpts()
	for range maxChain {
		res, err := d.InsertRiverJobTx(ctx, tx, args, &opts)
		if errors.Is(err, db.ErrNoRiverProducer) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("queue customer convergence: %w", err)
		}
		if res == nil || !res.UniqueSkippedAsDuplicate || res.Job == nil || res.Job.State != rivertype.JobStateRunning {
			return nil
		}
		args.After = res.Job.ID
	}
	return fmt.Errorf("queue customer convergence: %d jobs already running for customer %s", maxChain, customer)
}
