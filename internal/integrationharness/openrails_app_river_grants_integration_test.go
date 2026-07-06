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

// TestFullStackServesAndDrainsRiverAsOpenrailsApp proves #764: migration
// 0007_openrails_app_grants must give the openrails_app role everything the
// runtime actually needs on `public` (River) and `profiles` (AuthKit), not
// just the `openrails` schema migration 0001 already covers.
//
// StartStandalone already connects as openrails_app (dbtest.SharedRLSPostgres's
// app DSN) — that part is old news. WithWorkers is the new ingredient: no
// existing test starts the real in-process River workers against that role.
// The one pre-existing WithWorkers() caller (tests/testcontainer_suite.go)
// deliberately boots over the SUPER/BYPASSRLS DSN instead, so River's own
// tables have never been exercised end to end under the role production
// actually connects as. river.Client.Start alone does leader election + queue
// registration (writes to river_leader/river_queue) before any job is ever
// enqueued; enqueuing and draining one job exercises river_job end to end
// (INSERT to enqueue, SELECT+UPDATE to claim and finalize). AuthKit/profiles
// access is exercised by the same boot for free: minting the API key below
// (and StartStandalone's own bootstrap) round-trips through the real control
// plane's AuthKit core, which lives entirely in the `profiles` schema.
func TestFullStackServesAndDrainsRiverAsOpenrailsApp(t *testing.T) {
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

	// River: enqueue a resume job for a subscription id that does not exist.
	// The worker's own not-found handling (jobs_subscription_manage.go) treats
	// this as a graceful no-op success, so this needs no product/price/
	// customer/subscription fixtures at all — the only thing under test is
	// whether river_job/river_queue/river_leader are actually usable by
	// openrails_app end to end. If any of those grants were missing, either
	// the Insert below fails outright (a real Postgres permission-denied
	// error) or no worker ever claims the job and the Eventually poll times
	// out — both are unambiguous failures of this test.
	producer := surface.App().Runtime.RiverProducer
	require.NotNil(t, producer, "River producer must be initialized")
	_, err := producer.Insert(ctx, riverjobs.ResumeSubscriptionArgs{
		UserID:         uuid.NewString(),
		SubscriptionID: uuid.New(),
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
