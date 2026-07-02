//go:build integration

package tests

// River stripe-webhooks worker wrapper (#694 GAP 3): enqueue ⇒ worker picks up
// ⇒ handler invoked, proven by its observable effects. The apply path for
// inbound Stripe events is covered elsewhere; THIS job is the managed
// webhook-endpoint reconciler (jobs_stripe_webhooks.go): for every active
// stripe rail_merchant_accounts row it find-or-creates the OpenRails-managed
// Stripe webhook endpoint and persists the returned signing secret into the
// merchant secret store. Real River (in-process workers on real Postgres),
// real merchant catalog + secret store; the only fake is the Stripe wire
// server behind the stripeapi choke-point transport.

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func TestStripeWebhookReconcileRiverWorker(t *testing.T) {
	fake := newFakeStripeAPI(t)
	suite := setupTestSuite(t)
	// PublicStripeWebhookURL only registers endpoints for a public https base.
	suite.Config.APIURL = "https://api.openrails-e2e.example.com"

	ctx := dbtest.WithTestMerchant(context.Background())
	env := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	const accountID = "acct_river_e2e"
	suite.seedRailMerchantAccountWithEvidence(ctx, "stripe", env, accountID, "")

	keyName, err := merchants.RailMerchantAccountSecretName("stripe", env, accountID, "secret_key")
	require.NoError(t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, keyName, "sk_test_river_e2e")
	require.NoError(t, err)

	client := suite.GetRiverClient()
	require.NotNil(t, client, "River client must be running")

	res, err := client.Insert(context.Background(), riverjobs.StripeWebhookReconcileArgs{}, &river.InsertOpts{
		Queue: riverjobs.QueueBilling,
	})
	require.NoError(t, err, "enqueue stripe webhook reconcile job")

	// Wait for THIS job (not a completed-count — periodic jobs also complete).
	deadline := time.Now().Add(15 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		require.NoError(t, suite.Pool.QueryRow(context.Background(),
			"SELECT state FROM "+config.RiverSchema+".river_job WHERE id = $1", res.Job.ID).Scan(&state))
		if state == "completed" || state == "discarded" || state == "cancelled" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Equal(t, "completed", state,
		"stripe webhook reconcile job should be picked up and completed by the worker")

	// Handler effect 1: the managed endpoint was reconciled against Stripe
	// (list → miss → create), with the merchant-scoped webhook URL and the
	// pinned API version on the create form.
	fake.mu.Lock()
	require.NotZero(t, fake.webhookEndpointLists, "worker must list existing webhook endpoints")
	require.Len(t, fake.webhookEndpointCreates, 1, "worker must create the managed endpoint once")
	form := fake.webhookEndpointCreates[0]
	assert.Equal(t,
		"https://api.openrails-e2e.example.com/v1/merchants/"+dbtest.TestMerchantSlug+"/webhooks/stripe/"+accountID,
		form.Get("url"))
	assert.Equal(t, stripeapi.APIVersion, form.Get("api_version"))
	assert.Equal(t, "true", form.Get("metadata[openrails_managed]"))
	assert.NotEmpty(t, form["enabled_events[]"], "endpoint must subscribe to the handled event types")
	fake.mu.Unlock()

	// Handler effect 2: the returned signing secret was persisted so inbound
	// deliveries to this account verify.
	secretName, err := merchants.RailMerchantAccountSecretName("stripe", env, accountID, "webhook_signing_secret")
	require.NoError(t, err)
	sec, err := suite.App.Runtime.Merchants.Secrets().Get(ctx, dbtest.TestMerchantID, secretName)
	require.NoError(t, err)
	assert.Equal(t, "whsec_river_e2e", sec.Value)
}
