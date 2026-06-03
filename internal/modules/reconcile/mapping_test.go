package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// fakePriceResolver is a minimal in-memory priceResolver for mapping tests.
type fakePriceResolver struct {
	byID     map[uuid.UUID]*models.Price
	byStripe map[string]*models.Price
}

func (f *fakePriceResolver) GetByID(_ context.Context, id uuid.UUID) (*models.Price, error) {
	if p, ok := f.byID[id]; ok {
		return p, nil
	}
	return nil, errors.New("not found")
}

func (f *fakePriceResolver) GetByStripePriceID(_ context.Context, stripePriceID string) (*models.Price, error) {
	if p, ok := f.byStripe[stripePriceID]; ok {
		return p, nil
	}
	return nil, errors.New("not found")
}

func TestMapRemoteSubscriptionUsesInternalPriceID(t *testing.T) {
	priceID := uuid.New()
	prices := &fakePriceResolver{
		byID: map[uuid.UUID]*models.Price{priceID: {ID: priceID}},
	}
	remote := subscriptions.StripeRemoteSubscription{
		ID:            "sub_1",
		Status:        "active",
		StripePriceID: "price_stripe",
		Metadata: map[string]string{
			"user_id":           "user-123",
			"internal_price_id": priceID.String(),
		},
	}

	mapped, reason := mapRemoteSubscription(context.Background(), prices, remote)
	require.Empty(t, reason)
	require.NotNil(t, mapped)
	require.Equal(t, "user-123", mapped.UserID)
	require.Equal(t, priceID, mapped.PriceID)
}

func TestMapRemoteSubscriptionFallsBackToStripePriceID(t *testing.T) {
	priceID := uuid.New()
	prices := &fakePriceResolver{
		byStripe: map[string]*models.Price{"price_stripe": {ID: priceID}},
	}
	// internal_price_id is present but unknown to the catalog, so resolution
	// must fall through to the line-item Stripe price id.
	remote := subscriptions.StripeRemoteSubscription{
		ID:            "sub_2",
		Status:        "active",
		StripePriceID: "price_stripe",
		Metadata: map[string]string{
			"user_id":           "user-456",
			"internal_price_id": uuid.New().String(),
		},
	}

	mapped, reason := mapRemoteSubscription(context.Background(), prices, remote)
	require.Empty(t, reason)
	require.NotNil(t, mapped)
	require.Equal(t, "user-456", mapped.UserID)
	require.Equal(t, priceID, mapped.PriceID)
}

func TestMapRemoteSubscriptionSkipsMissingUser(t *testing.T) {
	prices := &fakePriceResolver{}
	remote := subscriptions.StripeRemoteSubscription{
		ID:       "sub_3",
		Status:   "active",
		Metadata: map[string]string{"internal_price_id": uuid.New().String()},
	}

	mapped, reason := mapRemoteSubscription(context.Background(), prices, remote)
	require.Nil(t, mapped)
	require.Contains(t, reason, "user_id")
}

func TestMapRemoteSubscriptionSkipsUnmappablePrice(t *testing.T) {
	prices := &fakePriceResolver{}
	remote := subscriptions.StripeRemoteSubscription{
		ID:            "sub_4",
		Status:        "active",
		StripePriceID: "price_unknown",
		Metadata:      map[string]string{"user_id": "user-789"},
	}

	mapped, reason := mapRemoteSubscription(context.Background(), prices, remote)
	require.Nil(t, mapped)
	require.Contains(t, reason, "price")
}

func TestDiffSubscriptions(t *testing.T) {
	remote := map[string]struct{}{"a": {}, "b": {}, "c": {}}
	local := map[string]struct{}{"b": {}, "c": {}, "d": {}}

	diff := diffSubscriptions(remote, local)
	require.Equal(t, []string{"a"}, diff.RemoteOnly)
	require.Equal(t, []string{"d"}, diff.LocalOnly)
	require.Equal(t, 2, diff.Matched)
}

func TestDiffSubscriptionsIdempotentWhenAllMatched(t *testing.T) {
	remote := map[string]struct{}{"a": {}, "b": {}}
	local := map[string]struct{}{"a": {}, "b": {}}

	diff := diffSubscriptions(remote, local)
	require.Empty(t, diff.RemoteOnly)
	require.Empty(t, diff.LocalOnly)
	require.Equal(t, 2, diff.Matched)
}

func TestZeroTimeNil(t *testing.T) {
	require.Nil(t, zeroTimeNil(time.Time{}))
	now := time.Date(2026, time.May, 23, 1, 2, 3, 0, time.UTC)
	got := zeroTimeNil(now)
	require.NotNil(t, got)
	require.Equal(t, now, *got)
}

func TestOptionsUserKnown(t *testing.T) {
	ctx := context.Background()

	t.Run("nil checker treats every user as known (legacy behavior)", func(t *testing.T) {
		ok, err := Options{}.userKnown(ctx, "anyone")
		if err != nil || !ok {
			t.Fatalf("nil checker: got ok=%v err=%v, want ok=true err=nil", ok, err)
		}
	})

	t.Run("delegates to the injected checker", func(t *testing.T) {
		opts := Options{UserExists: func(_ context.Context, userID string) (bool, error) {
			return userID == "known", nil
		}}
		if ok, _ := opts.userKnown(ctx, "known"); !ok {
			t.Error("known user should be reported as existing")
		}
		if ok, _ := opts.userKnown(ctx, "ghost"); ok {
			t.Error("unknown user should be reported as not existing")
		}
	})

	t.Run("propagates checker errors", func(t *testing.T) {
		want := errors.New("lookup failed")
		opts := Options{UserExists: func(_ context.Context, _ string) (bool, error) {
			return false, want
		}}
		if _, err := opts.userKnown(ctx, "x"); !errors.Is(err, want) {
			t.Fatalf("got err=%v, want %v", err, want)
		}
	})
}
