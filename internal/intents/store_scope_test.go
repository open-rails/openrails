package intents

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestEnqueueRequiresMatchingMerchantBeforePersistence(t *testing.T) {
	// A nil DB makes a mistaken attempt to persist fail the test immediately.
	store := NewStore(nil)
	a, b := merchant.ID(uuid.New()), uuid.New()
	p := EnqueueParams{MerchantID: b, IntentType: TypeNMIDeleteSubscription}
	_, err := store.Enqueue(context.Background(), p)
	require.ErrorIs(t, err, merchant.ErrNoMerchant)
	_, err = store.Enqueue(merchant.WithID(context.Background(), a), p)
	require.ErrorContains(t, err, "merchant does not match context")
}
