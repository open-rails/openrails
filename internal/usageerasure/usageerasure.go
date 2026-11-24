// Package usageerasure is the erasure of one customer's personal data from
// the usage event store: queued in the transaction that erases the customer,
// it clears its events' invoker and metadata and folds its history's
// invokers. Quantities and what posted stay.
package usageerasure

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
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
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}

// Erase erases the customer's usage from store under its spend lock in q's
// transaction: no intake adds to its history while the history is folded.
// A customer with no row has no usage to wait for.
func Erase(ctx context.Context, q *gen.Queries, store *eventstore.Store, merchantID, customer uuid.UUID) error {
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return store.EraseCustomer(ctx, billing.CustomerID(customer))
}
