// Package usageerasure is the erasure of one customer's personal data from
// the usage event store: queued in the transaction that erases the customer,
// it clears its events' invoker and metadata and folds its history's
// invokers. Quantities and what posted stay.
package usageerasure

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const Kind = "openrails.usage_erasure"

// Args names the customer whose usage is erased.
type Args struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	CustomerID uuid.UUID `json:"customer_id" river:"unique"`
}

func (Args) Kind() string { return Kind }

// InsertOpts: one erasure per customer while one is outstanding; River
// retries it until the store answers.
func (Args) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 25, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}
