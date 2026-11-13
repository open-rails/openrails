// Package balancelook is a look at one watched balance: queued with the change
// that crossed one of its notices, or for the instant the last look said to
// look again. It sits below the ledger so the ledger can queue it.
package balancelook

import (
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const Kind = "openrails.balance_look"

// Attempts: River retries a failed look within minutes; past them the watch
// stays due for the billing pass.
const Attempts = 5

// Args names the balance and the instant the look is for: the mark of the
// change that queued it, or the instant the last look named. A look per
// instant: a running look never absorbs a later one.
type Args struct {
	MerchantID uuid.UUID  `json:"merchant_id" river:"unique"`
	CustomerID uuid.UUID  `json:"customer_id" river:"unique"`
	Currency   string     `json:"currency" river:"unique"`
	At         *time.Time `json:"at,omitempty" river:"unique"`
}

func (Args) Kind() string { return Kind }

// InsertOpts: one look per balance and instant while one is outstanding. A
// look for a later instant is queued with ScheduledAt at it.
func (Args) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: Attempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}
