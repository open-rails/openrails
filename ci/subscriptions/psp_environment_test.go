//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// A PSP registered as test (as a deployment from before declared environments
// holds its sandbox.nmi.com account) and re-declared sandbox is the same PSP:
// test and sandbox are one money class. It keeps its id and its subscription,
// the stored environment is corrected in place, and its posture is verified
// again against the sandbox endpoints, which then carry its charges.
func TestPSPRedeclaredSandboxKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		nmi := psps["nmi"]
		nmi.Environment = openrails.EnvironmentTest
		psps["nmi"] = nmi
	}
	var mu sync.Mutex
	var requests []string
	w.nmi.Intercept(func(r *http.Request) bool {
		mu.Lock()
		requests = append(requests, r.URL.Host+r.URL.Path)
		mu.Unlock()
		return false
	}, nil)
	sent := func(prefix string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, request := range requests {
			if strings.HasPrefix(request, prefix) {
				n++
			}
		}
		return n
	}
	w.start()
	ctx := t.Context()
	rt := engine.Graph(w.rt).Runtime
	mid := w.client[embedded].MerchantID()
	id := w.psp["nmi"]
	stored := func() string {
		var environment string
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT environment FROM billing.psps WHERE id = $1`), id.UUID()).Scan(&environment))
		return environment
	}
	require.Equal(t, openrails.EnvironmentTest, stored())
	e := enroll(t, w, "nmi", embedded)
	require.Positive(t, sent("secure.nmi.com/api/query.php"), "a test PSP proves test mode on the regular gateway")
	require.Zero(t, sent("sandbox.nmi.com"))

	before, err := rt.MerchantConfig.Get(ctx, mid)
	require.NoError(t, err)
	next := before.Clone()
	doc := next.PSPs["nmi"]
	doc.Value.Environment = openrails.EnvironmentSandbox
	next.PSPs["nmi"] = doc
	rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
	served, err := rt.MerchantConfig.Reload(ctx, mid)
	require.NoError(t, err)
	require.Empty(t, served.Rejected, "test to sandbox is not an identity change")
	require.Equal(t, openrails.EnvironmentSandbox, stored(), "the stored environment is corrected in place")
	psp, err := w.client[remote].GetPSP(ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, psp.ID, "the PSP keeps its id")
	require.Equal(t, openrails.EnvironmentSandbox, psp.Environment)

	require.Eventually(t, func() bool { return sent("sandbox.nmi.com/api/v5/payments/auth") > 0 }, 20*time.Second, 20*time.Millisecond,
		"posture is verified again against the sandbox endpoints")
	require.Eventually(t, func() bool { return !rt.PSPPostureDisarmed(id.UUID()) }, 20*time.Second, 20*time.Millisecond)

	charged := sent("sandbox.nmi.com")
	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.True(t, e.periodEnd().After(end), "the existing subscription renews on the same PSP")
	require.Greater(t, sent("sandbox.nmi.com"), charged, "its renewal goes to the sandbox endpoints")
}
