package solanasubs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents/wake"
)

// PullType is the recurring on-chain pull: one durable operation per
// (subscription row, period anchor).
const PullType = "solana_pull"

// PullMargin is how long after next_pull_at an episode's pull is due:
// next_pull_at is the chain's clock, which may run behind ours.
const PullMargin = time.Minute

// PullKey addresses one pull episode: the anchor is the row's next_pull_at,
// which only advances when the period resolves (renewal, dunning reschedule).
func PullKey(rowID uuid.UUID, nextPullAt time.Time) string {
	return fmt.Sprintf("%s:%s:%d", PullType, rowID, nextPullAt.UTC().Unix())
}

// PullEpisode is the payload a pull episode is scheduled with.
type PullEpisode struct {
	SubscriptionPDA string    `json:"subscription_pda"`
	RowID           uuid.UUID `json:"row_id"`
	NextPullAt      time.Time `json:"next_pull_at"`
}

// write runs fn in a transaction (a savepoint in the caller's) that ends by
// scheduling row id's due pull episode, so the episode commits with the write
// that made it due.
func (r *SolanaSubscriptionRepo) write(ctx context.Context, merchantID, id uuid.UUID, fn func(ctx context.Context, q *gen.Queries) error) error {
	return r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		if err := fn(ctx, d.Gen(ctx)); err != nil {
			return err
		}
		return schedulePull(ctx, d, tx, merchantID, id)
	})
}

// schedulePull queues the row's due pull: a solana_pull operation due
// PullMargin after next_pull_at, with its wake. An inactive row has none; an
// episode whose operation already ended is left to it.
func schedulePull(ctx context.Context, d *db.DB, tx pgx.Tx, merchantID, id uuid.UUID) error {
	q := d.Gen(ctx)
	episode, err := q.GetSolanaPullEpisode(ctx, gen.GetSolanaPullEpisodeParams{MerchantID: merchantID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	payload, err := json.Marshal(PullEpisode{SubscriptionPDA: episode.SubscriptionPda, RowID: episode.ID, NextPullAt: episode.NextPullAt.UTC()})
	if err != nil {
		return err
	}
	reason := "solana recurring pull"
	op, err := q.EnqueueProviderIntent(ctx, gen.EnqueueProviderIntentParams{
		MerchantID: merchantID, Rail: "solana", IntentType: PullType, SubscriptionID: &episode.SubscriptionID,
		Payload: payload, IdempotencyKey: PullKey(episode.ID, episode.NextPullAt), NextAttemptAt: episode.NextPullAt.Add(PullMargin).UTC(),
		Origin: "system", OriginReason: &reason, PspID: &episode.PspID,
	})
	if err != nil {
		return fmt.Errorf("schedule solana pull: %w", err)
	}
	if op.Status != "pending" {
		// A claimed or unresolved attempt has its wake; a final one is its
		// operator's.
		return nil
	}
	return wake.Queue(ctx, d, tx, op)
}
