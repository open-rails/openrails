//go:build integration

package integrationharness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
)

// TestStripeTierChangeReplayAcrossDeployments: a Stripe tier change is one
// durable operation under one provider identity in every deployment shape.
// A succeeded change replays its stored receipt under the same key without a
// second Stripe push; a lost response answers 202 with the operation, the
// same key replays the live operation, another key is refused with it, and
// the restarted deployment's own verifier converges the change exactly once
// from the exact Stripe read-back; a definitive decline is coded and never
// resent. The provider is a loopback Stripe fake; no live Stripe behavior is
// proven.
func TestStripeTierChangeReplayAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	gateway := NewFakeStripeGateway(t)
	runProviderDeployments(t, h, config.ProviderSandboxConfig{StripeAPIURL: gateway.URL}, func(t *testing.T, d moneyDeployment) {
		client := d.client()

		// Success, then the stored receipt.
		fixture := h.SeedStripeTierSubscription(d.runtime(), d.merchant, gateway)
		key := "tier-" + uuid.NewString()[:8]
		request := openrails.ChangeTierRequest{PriceID: (fixture.ProPrice).String()}
		first, err := client.ChangeTier(ctx, fixture.Subscription, key, request)
		require.NoError(t, err)
		require.Equal(t, "succeeded", first.Status)
		require.Equal(t, "upgrade", first.Action)
		require.NotEmpty(t, first.OperationID)
		require.Equal(t, fixture.ProAmount, first.NextChargeAmount)
		posts := gateway.Posts("/v1/subscriptions/" + fixture.StripeSub)
		require.Len(t, posts, 1)
		require.Equal(t, first.OperationID+":update", posts[0].IdempotencyKey, "the provider identity is the operation")
		require.Equal(t, fixture.ProStripe, posts[0].Form.Get("items[0][price]"))
		require.Equal(t, first.OperationID, posts[0].Form.Get("metadata[openrails_tier_change]"))
		require.Equal(t, fixture.ProPrice, h.LocalSubscriptionPrice(fixture.SubscriptionID))
		again, err := client.ChangeTier(ctx, fixture.Subscription, key, request)
		require.NoError(t, err)
		require.Equal(t, first, again, "the same key answers the stored receipt")
		require.Len(t, gateway.Posts("/v1/subscriptions/"+fixture.StripeSub), 1, "a replay never pushes Stripe again")
		require.Equal(t, 1, h.TierChangeOperations(fixture.SubscriptionID))

		// Lost response: accepted, owned, converged once after a restart.
		lost := h.SeedStripeTierSubscription(d.runtime(), d.merchant, gateway)
		lostRequest := openrails.ChangeTierRequest{PriceID: (lost.ProPrice).String()}
		lostKey := "lost-" + uuid.NewString()[:8]
		// The live verifier must not receive the landed receipt until the
		// pending/replay assertions finish and the deployment has restarted.
		releaseReadback := gateway.HoldLostSubscriptionReadback(lost.StripeSub)
		defer releaseReadback()
		gateway.SetSubscriptionMode(lost.StripeSub, StripeWriteLostAfterLanding)
		pending, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
		require.NoError(t, err)
		require.Equal(t, "processing", pending.Status)
		op := h.LatestTierChangeOperation(lost.SubscriptionID)
		require.Equal(t, "unknown_needs_verify", op.Status)
		require.Equal(t, op.ID.String(), pending.OperationID)
		require.Equal(t, lost.BasicPrice, h.LocalSubscriptionPrice(lost.SubscriptionID), "nothing commits without a receipt")
		require.Equal(t, lost.ProStripe, gateway.Subscription(lost.StripeSub).PriceID, "the update landed at Stripe")
		replayed, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
		require.NoError(t, err)
		require.Equal(t, pending, replayed)
		_, err = client.ChangeTier(ctx, lost.Subscription, "other-"+uuid.NewString()[:8], lostRequest)
		requireRefusal(t, err, openrails.ErrConflict, openrails.CodeTierChangeInFlight)
		var refused *openrails.StatusError
		require.True(t, errors.As(err, &refused))
		require.Equal(t, op.ID.String(), refused.Metadata["operation_id"])
		require.Len(t, gateway.Posts("/v1/subscriptions/"+lost.StripeSub), 1, "nothing is resent while the outcome is unknown")

		d.stop()
		d.start()
		releaseReadback()
		client = d.client()
		h.MakeOperationDue(op.ID)
		h.FireProviderIntentVerify(h.Pool(), op.ID)
		require.Eventually(t, func() bool {
			return h.LatestTierChangeOperation(lost.SubscriptionID).Status == "succeeded"
		}, 90*time.Second, 500*time.Millisecond, "the restarted deployment's verifier settles from the exact read-back")
		require.Equal(t, lost.ProPrice, h.LocalSubscriptionPrice(lost.SubscriptionID))
		done, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
		require.NoError(t, err)
		require.Equal(t, "succeeded", done.Status)
		require.Equal(t, pending.OperationID, done.OperationID)
		require.Equal(t, "upgrade", done.Action)
		require.Equal(t, lost.ProPrice.String(), done.PriceID)
		require.Equal(t, lost.ProAmount, done.NextChargeAmount)
		doneAgain, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
		require.NoError(t, err)
		require.Equal(t, done, doneAgain, "after restart the settled operation replays the exact stored receipt")
		require.Len(t, gateway.Posts("/v1/subscriptions/"+lost.StripeSub), 1)
		require.Equal(t, 1, h.TierChangeOperations(lost.SubscriptionID))

		// A tier change without the client's key never mutates.
		unkeyed := h.SeedStripeTierSubscription(d.runtime(), d.merchant, gateway)
		_, err = client.ChangeTier(ctx, unkeyed.Subscription, "", openrails.ChangeTierRequest{PriceID: (unkeyed.ProPrice).String()})
		requireRefusal(t, err, openrails.ErrInvalid, openrails.CodeTierChangeIdempotencyKeyRequired)
		require.Empty(t, gateway.Posts("/v1/subscriptions/"+unkeyed.StripeSub))
		require.Equal(t, unkeyed.BasicPrice, h.LocalSubscriptionPrice(unkeyed.SubscriptionID))

		// A definitive decline is coded, replays as itself and is never resent.
		declined := h.SeedStripeTierSubscription(d.runtime(), d.merchant, gateway)
		declinedRequest := openrails.ChangeTierRequest{PriceID: (declined.ProPrice).String()}
		declinedKey := "decl-" + uuid.NewString()[:8]
		gateway.SetSubscriptionMode(declined.StripeSub, StripeWriteDecline)
		_, err = client.ChangeTier(ctx, declined.Subscription, declinedKey, declinedRequest)
		requireRefusal(t, err, openrails.ErrPaymentRefused, "insufficient_funds")
		require.Equal(t, "failed_terminal", h.LatestTierChangeOperation(declined.SubscriptionID).Status)
		_, err = client.ChangeTier(ctx, declined.Subscription, declinedKey, declinedRequest)
		requireRefusal(t, err, openrails.ErrPaymentRefused, "insufficient_funds")
		require.Len(t, gateway.Posts("/v1/subscriptions/"+declined.StripeSub), 1)
		require.Equal(t, declined.BasicPrice, h.LocalSubscriptionPrice(declined.SubscriptionID))
	})
}
