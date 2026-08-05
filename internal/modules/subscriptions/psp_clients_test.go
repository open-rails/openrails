package subscriptions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// recordingNMISource records the identity NMIClientForExistingSubscription
// resolves with — the #788 seam contract: the subscription's merchant plus its
// stamped provenance account (archived stays addressable, #655) pass through
// to the ONE store-armed resolver; nothing else picks the client.
type recordingNMISource struct {
	client     *nmi.NMIClient
	err        error
	merchantID uuid.UUID
	stamped    *uuid.UUID
}

func (r *recordingNMISource) ResolveNMIClient(_ context.Context, merchantID uuid.UUID, stamped *uuid.UUID) (*nmi.NMIClient, bool, error) {
	r.merchantID = merchantID
	r.stamped = stamped
	if r.err != nil {
		return nil, false, r.err
	}
	if r.client == nil {
		return nil, false, nil
	}
	return r.client, true, nil
}

func TestNMIClientForExistingSubscriptionResolvesThroughStampedScope(t *testing.T) {
	mid := uuid.New()
	stamped := uuid.New()
	client := &nmi.NMIClient{}
	src := &recordingNMISource{client: client}

	got, _, ok, err := NMIClientForExistingSubscription(context.Background(), src, &models.Subscription{
		Rail:       models.RailNMI,
		MerchantID: mid,
		PspID:      stamped,
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Same(t, client, got)
	require.Equal(t, mid, src.merchantID, "the subscription's merchant scopes the resolution")
	require.NotNil(t, src.stamped)
	require.Equal(t, stamped, *src.stamped, "the stamped provenance account pins the client (#655/#704)")
}

func TestNMIClientForExistingSubscriptionFailsClosed(t *testing.T) {
	// nil resolver: error, never a silent client.
	_, _, _, err := NMIClientForExistingSubscription(context.Background(), nil, &models.Subscription{Rail: models.RailNMI})
	require.Error(t, err)

	// resolver error propagates (fail closed).
	boom := errors.New("store unavailable")
	_, _, ok, err := NMIClientForExistingSubscription(context.Background(), &recordingNMISource{err: boom}, &models.Subscription{Rail: models.RailNMI})
	require.ErrorIs(t, err, boom)
	require.False(t, ok)

	// no armed account: ok=false, nil error (caller decides skip vs reject).
	_, _, ok, err = NMIClientForExistingSubscription(context.Background(), &recordingNMISource{}, &models.Subscription{Rail: models.RailNMI})
	require.NoError(t, err)
	require.False(t, ok)
}
