//go:build integration

package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestSaleStaleUnsentVerificationCannotExpireSubmittedPayment(t *testing.T) {
	f := newSaleIntentFixture(t)
	store := intents.NewStore(f.db)
	now := time.Now().UTC()
	expiry := now.Add(time.Minute)
	row, err := store.Enqueue(f.ctx, intents.EnqueueParams{MerchantID: f.merchantID.UUID(), Provider: "nmi", PspID: f.payload.Instrument.PSPID, PriceID: &f.priceID, IntentType: payments.TypeNMISale, IdempotencyKey: NMISaleIdempotencyKey(uuid.NewString()), Payload: f.payload, Origin: intents.OriginUser, NextAttemptAt: now, ExpiresAt: &expiry})
	require.NoError(t, err)
	claimed, ok, err := store.ClaimByID(f.ctx, row.ID, now, now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkUnknown(f.ctx, row.ID, now, "claim owner paused before submission", nil))
	handler := f.runner.Registry.Lookup(payments.TypeNMISale)
	stale := handler.Verify(f.ctx, row)
	require.Equal(t, intents.OutcomeRetryable, stale.Class)
	// The old claim owner resumes after the verifier read but before its write.
	f.gateway.saleMode.Store("ambiguous500")
	f.gateway.hidden.Store(true)
	sent := handler.Execute(db.WithPSPID(f.ctx, *claimed.PspID), claimed)
	require.Equal(t, intents.OutcomeAmbiguous, sent.Class)
	require.EqualValues(t, 1, f.gateway.saleCalls.Load())
	transitionErr := store.MarkFailedRetryable(f.ctx, row.ID, now.Add(time.Hour), stale.Reason)
	require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(c context.Context) error { _, err := store.ExpireOverdue(c, now.Add(2*time.Hour)); return err }))
	current, err := store.Get(f.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status, "stale unsent result cannot make a submitted sale retryable/expirable")
	require.Error(t, transitionErr, "canonical submission must reject the stale transition")
}
