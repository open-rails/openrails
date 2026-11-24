package merchants

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// UsagePurgeArgs is the job that deletes a purged merchant's usage from the
// event store. The purge queues it in its own transaction, so a deletion the
// store could not take then is retried after the merchant is tombstoned.
type UsagePurgeArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
}

func (UsagePurgeArgs) Kind() string { return "openrails.merchant_usage_purge" }

func (UsagePurgeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 25, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}
