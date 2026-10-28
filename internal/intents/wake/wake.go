// Package wake is a provider operation's River job: the wake that runs it,
// committed in the transaction that leaves the operation live. It sits below
// intents so a producer intents depends on can schedule an operation too.
package wake

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

const Kind = "openrails.provider_operation"

// Args carries identities only. The ledger decides what is authorized.
type Args struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	IntentID   uuid.UUID `json:"intent_id" river:"unique"`
}

func (Args) Kind() string { return Kind }

// Opts schedules a wake at at.
func Opts(at time.Time) *river.InsertOpts {
	return &river.InsertOpts{Queue: "billing", ScheduledAt: at.UTC(),
		// Business/infra waiting uses Snooze (which does not consume attempts).
		// Leave maximal room for River's crash rescue; never use its usual finite
		// provider-error retry budget to abandon an accepted financial operation.
		MaxAttempts: math.MaxInt16,
	}
}

// At is when a live operation is next due: its next attempt, or a producer's
// hold on a pending one when later.
func At(row gen.BillingProviderIntent) time.Time {
	at := row.NextAttemptAt
	if row.Status == "pending" && row.LeaseExpiresAt != nil && row.LeaseExpiresAt.After(at) {
		at = *row.LeaseExpiresAt
	}
	return at
}

// Queue inserts row's wake at At in tx, the transaction that wrote row.
func Queue(ctx context.Context, d *db.DB, tx pgx.Tx, row gen.BillingProviderIntent) error {
	_, err := d.InsertRiverJobTx(ctx, tx, Args{MerchantID: row.MerchantID, IntentID: row.ID}, Opts(At(row)))
	return err
}
