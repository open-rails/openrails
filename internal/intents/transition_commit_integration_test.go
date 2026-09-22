//go:build integration

package intents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestSuccessorAcknowledgementRequiresTopLevelCommit(t *testing.T) {
	d := dbtest.OpenOneConnAppDB(t)
	dbtest.EnsureTestMerchant(t.Context(), t, d.Pool())
	mid := dbtest.TestMerchantID
	ctx := merchant.WithID(t.Context(), mid)
	psp := dbtest.EnsureTestPSP(ctx, t, d.Pool(), mid.UUID(), "nmi")
	store := NewStore(d)
	now := time.Now().UTC()
	row, err := store.Enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: "nmi", PspID: psp, IntentType: "test_commit_ack", IdempotencyKey: uuid.NewString(), Origin: OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	_, claimed, err := store.ClaimByID(ctx, row.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	acknowledgements := 0
	runner := &Runner{OnSuccessorCommitted: func(owner, id uuid.UUID) {
		require.Equal(t, mid.UUID(), owner)
		require.Equal(t, row.ID, id)
		acknowledgements++
	}}
	rollback := errors.New("host rollback")
	for _, abort := range []bool{true, false} {
		err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			require.NoError(t, NewStore(db.NewWithPgxTx(tx)).MarkUnknown(runner.transitionContext(ctx), row.ID, now, "nested", nil))
			require.Zero(t, acknowledgements, "savepoint release is not a durable successor acknowledgement")
			if abort {
				return rollback
			}
			return nil
		})
		if abort {
			require.ErrorIs(t, err, rollback)
		} else {
			require.NoError(t, err)
		}
		require.Zero(t, acknowledgements, "a Store may not acknowledge the host's later commit")
	}
	// Even a root Store must not turn a marked ambient context into a commit proof.
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		require.ErrorIs(t, store.MarkUnknown(runner.transitionContext(ctx), row.ID, now, "ambient root", nil), db.ErrCallerTransaction)
		require.Zero(t, acknowledgements)
		return nil
	}))
	require.NoError(t, store.MarkUnknown(runner.transitionContext(ctx), row.ID, now, "top level", nil))
	require.Equal(t, 1, acknowledgements)
	// Terminal/zero-row writes cannot claim a handoff.
	require.NoError(t, store.MarkSucceeded(ctx, row.ID, now, nil))
	require.NoError(t, store.MarkUnknown(runner.transitionContext(ctx), row.ID, now, "stale", nil))
	require.Equal(t, 1, acknowledgements)
	row, err = store.Enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: "nmi", PspID: psp, IntentType: "test_commit_handler_context", IdempotencyKey: uuid.NewString(), Origin: OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	runner.Store = store
	runner.Registry = NewRegistry(&commitContextHandler{t: t})
	runner.Config = &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	current, err := runner.ExecuteByID(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, current.Status)
	require.Equal(t, 2, acknowledgements)
	current, err = runner.VerifyByID(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, current.Status)
	require.Equal(t, 2, acknowledgements)
}

type commitContextHandler struct{ t *testing.T }

func (*commitContextHandler) Type() string { return "test_commit_handler_context" }
func (*commitContextHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *commitContextHandler) Execute(ctx context.Context, _ gen.OpenrailsRailIntent) Outcome {
	require.Nil(h.t, ctx.Value(successorCommitContextKey{}), "handler cannot acknowledge its own nested transaction")
	return Ambiguous("readback required")
}
func (h *commitContextHandler) Verify(ctx context.Context, _ gen.OpenrailsRailIntent) Outcome {
	require.Nil(h.t, ctx.Value(successorCommitContextKey{}), "verifier handler cannot acknowledge a savepoint")
	return Succeeded(nil)
}
func (*commitContextHandler) Backoff(int32) time.Duration { return time.Second }
