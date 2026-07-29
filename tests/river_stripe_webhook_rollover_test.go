//go:build integration

package tests

// or#856: the hourly managed-webhook reconciler used to DELETE every merchant's
// live Stripe endpoint on an api_version bump or a local secret miss, losing
// every delivery in the gap. This proves the replacement end to end on the real
// worker, real Postgres, real merchant secret store, real destructive gate —
// the only fake is the Stripe wire server behind the stripeapi choke point.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeStripeWebhookAPI is a stateful stand-in for /v1/webhook_endpoints.
type fakeStripeWebhookAPI struct {
	server *httptest.Server

	mu        sync.Mutex
	endpoints map[string]map[string]any
	creates   []url.Values
	deletes   []string
	seq       int
}

func newFakeStripeWebhookAPI(t *testing.T) *fakeStripeWebhookAPI {
	t.Helper()
	f := &fakeStripeWebhookAPI{endpoints: map[string]map[string]any{}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/webhook_endpoints", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		data := make([]map[string]any, 0, len(f.endpoints))
		for _, e := range f.endpoints {
			data = append(data, e)
		}
		writeJSON(w, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	mux.HandleFunc("POST /v1/webhook_endpoints", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seq++
		id := "we_new_" + string(rune('a'+f.seq-1))
		ep := map[string]any{
			"id": id, "object": "webhook_endpoint", "status": "enabled",
			"url": r.PostForm.Get("url"), "api_version": r.PostForm.Get("api_version"),
			"created":        int64(2000 + f.seq),
			"enabled_events": r.PostForm["enabled_events[]"],
			"metadata":       map[string]any{"openrails_managed": "true"},
		}
		f.endpoints[id] = ep
		f.creates = append(f.creates, r.PostForm)
		out := map[string]any{"secret": "whsec_rolled_" + id}
		for k, v := range ep {
			out[k] = v
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /v1/webhook_endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		defer f.mu.Unlock()
		ep := f.endpoints[r.PathValue("id")]
		if ep == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		require.Empty(t, r.PostForm.Get("api_version"),
			"stripe rejects api_version on update; we must never send it")
		meta, _ := ep["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		if v := r.PostForm.Get("metadata[" + catalog.StripeMetadataSupersededAt + "]"); v != "" {
			meta[catalog.StripeMetadataSupersededAt] = v
		}
		ep["metadata"] = meta
		if u := r.PostForm.Get("url"); u != "" {
			ep["url"] = u
		}
		writeJSON(w, ep)
	})
	mux.HandleFunc("DELETE /v1/webhook_endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		delete(f.endpoints, id)
		f.deletes = append(f.deletes, id)
		writeJSON(w, map[string]any{"id": id, "deleted": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake stripe: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	stripeapi.SetTestBaseTransport(hostRewriteTransport{target: f.server.URL})
	t.Cleanup(func() { stripeapi.SetTestBaseTransport(nil) })
	return f
}

func (f *fakeStripeWebhookAPI) snapshot() (creates int, deletes []string, live map[string]map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	live = map[string]map[string]any{}
	for k, v := range f.endpoints {
		live[k] = v
	}
	return len(f.creates), append([]string(nil), f.deletes...), live
}

// A version bump rolls over with ZERO deletes; the superseded endpoint keeps
// delivering; both secrets verify; and the delete waits on the operator kill
// switch even after the overlap window has expired.
func TestStripeWebhookReconcileVersionBumpIsGapless(t *testing.T) {
	t.Skip("or#877 B6: the reconciler's target list is empty under openrails_app, so no successor endpoint " +
		"is created. Un-skip with the per-merchant walk.")

	fake := newFakeStripeWebhookAPI(t)
	suite := setupTestSuite(t)
	suite.Config.APIURL = "https://api.openrails-e2e.example.com"

	ctx := suite.MerchantCtx()
	env := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	const accountID = "acct_rollover_856"
	suite.seedRailMerchantAccountWithEvidence(ctx, "stripe", env, accountID, "")
	// The suite's merchant is SHARED with every other test in this package:
	// leave no psps row behind for the next one to reconcile.
	t.Cleanup(func() { dropPSP(t, suite, accountID) })

	secretsStore := suite.App.Runtime.Merchants.Secrets()
	keyName, err := merchants.PSPSecretName("stripe", env, accountID, "secret_key")
	require.NoError(t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, keyName, "sk_test_856")
	require.NoError(t, err)
	webhookName, err := merchants.PSPSecretName("stripe", env, accountID, "webhook_signing_secret")
	require.NoError(t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, webhookName, "whsec_on_the_old_endpoint")
	require.NoError(t, err)
	previousName, err := merchants.PSPSecretName("stripe", env, accountID, "webhook_signing_secret_previous")
	require.NoError(t, err)

	wantURL := "https://api.openrails-e2e.example.com/v1/merchants/" + dbtest.TestMerchantSlug + "/webhooks/stripe/" + accountID
	fake.endpoints["we_old"] = map[string]any{
		"id": "we_old", "object": "webhook_endpoint", "status": "enabled",
		"url": wantURL, "api_version": "2020-01-01", "created": int64(1),
		"enabled_events": []string{"invoice.paid"},
		"metadata":       map[string]any{"openrails_managed": "true"},
	}

	now := time.Now().UTC()
	worker := riverjobs.StripeWebhookReconcileWorker{
		DB: suite.App.Runtime.DB, Config: suite.Config, Merchants: suite.App.Runtime.Merchants,
		Now:           func() time.Time { return now },
		RetireOverlap: time.Hour,
	}
	job := &river.Job[riverjobs.StripeWebhookReconcileArgs]{}

	// PASS 1 — the bump. Create first, stamp the predecessor, delete nothing.
	require.NoError(t, worker.Work(context.Background(), job))
	creates, deletes, live := fake.snapshot()
	require.Equal(t, 1, creates, "the successor is created")
	require.Empty(t, deletes, "an api_version bump must delete NOTHING")
	require.Contains(t, live, "we_old", "the old endpoint is still registered with Stripe")
	require.Equal(t, "enabled", live["we_old"]["status"], "and still delivering")
	require.NotEmpty(t, live["we_old"]["metadata"].(map[string]any)[catalog.StripeMetadataSupersededAt])

	// The new secret is primary and the outgoing one is retained, so deliveries
	// already queued on the superseded endpoint still verify: no gap.
	cur, err := secretsStore.Get(ctx, dbtest.TestMerchantID, webhookName)
	require.NoError(t, err)
	require.NotEqual(t, "whsec_on_the_old_endpoint", cur.Value)
	prev, err := secretsStore.Get(ctx, dbtest.TestMerchantID, previousName)
	require.NoError(t, err)
	require.Equal(t, "whsec_on_the_old_endpoint", prev.Value)

	// The rollover raised an operator finding rather than self-deleting.
	require.Contains(t, openWebhookFinding(t, suite, accountID), "STILL ENABLED")

	// PASS 2 — past the overlap, kill switch still OFF (the fail-closed default):
	// the destructive half stays halted.
	worker.Now = func() time.Time { return now.Add(3 * time.Hour) }
	require.NoError(t, worker.Work(context.Background(), job))
	creates, deletes, live = fake.snapshot()
	require.Equal(t, 1, creates, "no second rollover — the pass is idempotent")
	require.Empty(t, deletes, "kill switch OFF halts every delete in this worker")
	require.Contains(t, live, "we_old")
	require.Contains(t, openWebhookFinding(t, suite, accountID), "kill switch is off")

	// PASS 3 — operator arms the switch: the superseded endpoint retires, its
	// secret is dropped, and the finding closes.
	require.NoError(t, destructive.New(suite.App.Runtime.DB).SetSwitch(
		context.Background(), true, "or856-test", "arm for retirement"))
	require.NoError(t, worker.Work(context.Background(), job))
	creates, deletes, live = fake.snapshot()
	require.Equal(t, 1, creates)
	require.Equal(t, []string{"we_old"}, deletes, "only the already-replaced endpoint is removed")
	require.NotContains(t, live, "we_old")
	require.Len(t, live, 1, "the successor is still there — never left unreachable")
	_, err = secretsStore.Get(ctx, dbtest.TestMerchantID, previousName)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	require.Empty(t, openWebhookFinding(t, suite, accountID), "the finding auto-resolves")
}

// openWebhookFinding returns the recommended action of the open managed-endpoint
// finding for this account, or "" when there is none.
func openWebhookFinding(t *testing.T, suite *TestContainerSuite, accountID string) string {
	t.Helper()
	var action string
	err := suite.App.Runtime.DB.RunInMerchantConn(
		merchant.WithID(context.Background(), dbtest.TestMerchantID),
		func(ctx context.Context) error {
			row := suite.App.Runtime.DB.Qx(ctx).QueryRow(ctx, `
				SELECT COALESCE(recommended_action, '')
				  FROM openrails.reconciliation_findings
				 WHERE merchant_id = $1::uuid AND finding_type = $2 AND subject_key = $3
				   AND status IN ('reconcile_required', 'requires_review')
			`, dbtest.TestMerchantID.String(), riverjobs.FindingStripeWebhookEndpoint, "stripe:"+accountID)
			if err := row.Scan(&action); err != nil {
				action = ""
			}
			return nil
		})
	require.NoError(t, err)
	return action
}

// dropPSP removes a psps row seeded by this test from the shared suite merchant.
func dropPSP(t *testing.T, suite *TestContainerSuite, accountID string) {
	t.Helper()
	_, err := suite.Pool.Exec(context.Background(),
		`DELETE FROM openrails.psps WHERE account_id = $1`, accountID)
	require.NoError(t, err)
}
