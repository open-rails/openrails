package main

import (
	"context"
	"errors"
	"testing"

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
