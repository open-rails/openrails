//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Prove the unprivileged standalone runtime can serve authenticated HTTP and
// enqueue, claim and complete real River work using its cross-schema grants.
func testFullStackServesAndDrainsRiverAsOpenrailsApp(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd", WithWorkers())

	// AuthKit/profiles: minting a real API key round-trips through AuthKit
	// core's profiles-schema tables (actors, permission groups, API keys).
	token := surface.MintAPIKey(dbtest.TestMerchantSlug, "river-grants-"+uuid.NewString(),
		[]string{controlplane.PermMerchantSubscriptionsRead})
	require.NotEmpty(t, token)

	// "serves": the standalone HTTP surface, connected strictly as
	// openrails_app, answers a real request.
	status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/subscriptions", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// River: enqueue a resume job for a customer with nothing to resume. That
	// path (no subscription id ⇒ "latest resumable cancelled" lookup ⇒ none)
	// is a genuine no-op success, so this needs no product/price/customer/
	// subscription fixtures at all — the only thing under test is whether
	// river_job/river_queue/river_leader are actually usable by openrails_app
	// end to end. If any of those grants were missing, either the Insert below
	// fails outright (a real Postgres permission-denied error) or no worker
	// ever claims the job and the Eventually poll times out — both are
	// unambiguous failures of this test.
	//
	// It deliberately no longer enqueues an UNKNOWN subscription id: that used
	// to "complete" only because the worker could not see the row and called
	// that success (or#877 B7). It is an error now.
	producer := surface.App().Runtime.RiverProducer
	require.NotNil(t, producer, "River producer must be initialized")
	_, err := producer.Insert(ctx, riverjobs.ResumeSubscriptionArgs{
		MerchantID: dbtest.TestMerchantID.UUID(),
		UserID:     uuid.NewString(),
	}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(t, err, "enqueueing a River job as openrails_app must succeed (INSERT on river_job)")

	require.Eventually(t, func() bool {
		var state string
		err := h.sharedPool().QueryRow(ctx,
			"SELECT state FROM "+config.RiverSchema+".river_job WHERE kind = $1 ORDER BY id DESC LIMIT 1",
			riverjobs.KindSubscriptionResume,
		).Scan(&state)
		return err == nil && state == "completed"
	}, 15*time.Second, 100*time.Millisecond,
		"the openrails_app-connected worker pool must claim and complete the enqueued River job")
}
