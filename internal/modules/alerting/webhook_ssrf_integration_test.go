//go:build integration

package alerting_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile"
)

// #SEC-21: a merchant-registered alert sink is an SSRF primitive — anyone with
// merchant:settings:update could make OpenRails POST from inside the network.
// Registration must refuse a non-public destination outright. (A destination
// given as a NAME is only decidable at the dialer, which the httpx tests cover.)
func TestCreateWebhookRejectsInternalDestinations(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	svc := newService(t, appDB, nil) // loopback-allowing policy; link-local still blocked

	inConn(t, appDB, mid, func(ctx context.Context) {
		for _, target := range []string{
			"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
			"http://10.0.0.5:8500/v1/kv/",
			"http://100.64.0.1/",
			"https://[fd00::1]/",
			"http://192.168.1.1/",
		} {
			_, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "ssrf", URL: target})
			require.Error(t, err, "%s must be refused at registration", target)
			require.Contains(t, err.Error(), "publicly routable")
		}
	})
}

// A sink on a reachable host must not be able to 302 the delivery into
// link-local, and the tenant-visible Detail must not describe what happened on
// the internal network.
func TestWebhookDeliveryDoesNotFollowRedirectIntoLinkLocal(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	svc := newService(t, appDB, nil)

	var hits int32
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()

	inConn(t, appDB, mid, func(ctx context.Context) {
		_, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "redirector", URL: redirector.URL})
		require.NoError(t, err)
		store := &reconcile.PGStore{DB: appDB}
		run, err := store.CreateRun(ctx, reconcile.ModeAdvisory, []reconcile.Provider{reconcile.ProviderNMI}, nil, nil)
		require.NoError(t, err)
		rec, err := store.UpsertFinding(ctx, run, reconcile.Finding{Provider: reconcile.ProviderNMI, Type: reconcile.FindingChargebackActiveSub, SubjectKey: uuid.NewString(), Severity: reconcile.SeverityMedium, Status: reconcile.FindingStatusRequiresReview})
		require.NoError(t, err)
		require.NoError(t, svc.NotifyFinding(ctx, rec))
	})

	require.Positive(t, atomic.LoadInt32(&hits), "the origin was contacted; the redirect target was not")
}
