//go:build e2e && integration

package subscriptions_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/vaulttest"
)

// An edit through one replica reaches another at once: the writer announces
// it and the others reload that merchant, without waiting for the recheck.
func TestVaultEditReachesEveryReplica(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	sibling := w.sibling()
	seen, err := sibling.client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)

	grace := 9
	updated, err := w.client[embedded].UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
		ExpectedRevision: &seen.Revision, Settings: &billing.MerchantSettings{ArrearsGraceDays: &grace},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := sibling.client.GetMerchantConfiguration(ctx)
		return err == nil && got.Revision == updated.Revision && got.Settings.ArrearsGraceDays != nil && *got.Settings.ArrearsGraceDays == 9
	}, 5*time.Second, 50*time.Millisecond, "the other replica reloads on the announcement, well inside the recheck")
}

// A PSP armed through one replica is offered by another's checkout without
// a restart: each replica verifies a PSP again when its document changes.
func TestPSPArmedOnOneReplicaServesOnAnother(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.vault = vaulttest.New(t)
	const liveKey = "live-nmi-key"
	w.declare = func(psps map[string]openrails.PSPConfig) {
		nmi := psps["nmi"]
		nmi.Secrets["security_key"] = liveKey
		clear(psps)
		psps["nmi"] = nmi
	}
	// The gateway declines the test-mode probe for liveKey: a live account.
	w.nmi.Intercept(func(r *http.Request) bool {
		return strings.HasSuffix(r.URL.Path, "/payments/auth") && r.Header.Get("Authorization") == liveKey
	}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		body := `{"object":"transaction","id":"probe-live","response":"2","response_code":"200","response_text":"DECLINE"}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(w.nmi.ClearIntercepts)
	w.start()
	ctx := t.Context()
	other := w.sibling()
	nmi := w.psp["nmi"]
	require.Eventually(t, func() bool { return engine.Graph(other.rt).Runtime.PSPPostureDisarmed(nmi.UUID()) },
		10*time.Second, 50*time.Millisecond, "a live account is disarmed in a sandbox deployment")

	product, err := w.client[embedded].CreateProduct(ctx, billing.CreateProductParams{Key: "armed", DisplayName: "Armed", Entitlements: []string{"content:armed"}})
	require.NoError(t, err)
	price, err := w.client[embedded].CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "armed-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	checkout := func() error {
		_, err := other.client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{
			Customer: billing.CheckoutCustomerIdentity{ID: cid(uuid.NewString()), VerifiedEmail: "armed@example.test"}, PriceID: price.ID, SuccessURL: "https://e2e.test/return",
		})
		return err
	}
	require.ErrorIs(t, checkout(), billing.ErrInvalid, "nothing sells through a disarmed PSP")

	psp, err := w.client[embedded].GetPSP(ctx, nmi)
	require.NoError(t, err)
	_, err = w.client[embedded].UpdatePSP(ctx, nmi, billing.UpdatePSPParams{ExpectedRevision: &psp.Revision, Credentials: map[string]string{"security_key": "e2e-nmi-key"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return checkout() == nil }, 10*time.Second, 50*time.Millisecond, "the other replica sells through the armed PSP")
	require.False(t, engine.Graph(other.rt).Runtime.PSPPostureDisarmed(nmi.UUID()))
}

// Rotating one PSP's credential writes that PSP's document alone.
func TestPSPRotationTouchesOneDocument(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	root := "openrails/merchants/" + engine.Graph(w.rt).Runtime.ConfiguredMerchant().UUID().String() + "/"
	versions := func() map[string]int {
		out := map[string]int{}
		for _, path := range []string{"merchant", "psps/stripe", "psps/nmi", "psps/ccbill"} {
			var doc map[string]any
			out[path] = w.vault.Get(t, root+path, &doc)
		}
		return out
	}
	before := versions()
	psp, err := w.client[remote].GetPSP(ctx, w.psp["nmi"])
	require.NoError(t, err)
	_, err = w.client[remote].UpdatePSP(ctx, w.psp["nmi"], billing.UpdatePSPParams{ExpectedRevision: &psp.Revision,
		Credentials: map[string]string{"webhook_signing_secret": "nmi_rotated_e2e_secret"}})
	require.NoError(t, err)
	after := versions()
	require.Equal(t, before["psps/nmi"]+1, after["psps/nmi"])
	for _, path := range []string{"merchant", "psps/stripe", "psps/ccbill"} {
		require.Equal(t, before[path], after[path], "%s is untouched", path)
	}
	var doc struct {
		Secrets map[string]string `json:"secrets"`
	}
	w.vault.Get(t, root+"psps/nmi", &doc)
	require.Equal(t, "nmi_rotated_e2e_secret", doc.Secrets["webhook_signing_secret"])
	require.Equal(t, whsecNMI, doc.Secrets["webhook_signing_secret_previous"], "the outgoing secret verifies through the overlap")

	_, err = w.client[remote].UpdatePSP(ctx, w.psp["nmi"], billing.UpdatePSPParams{ExpectedRevision: &psp.Revision,
		Credentials: map[string]string{"webhook_signing_secret": "nmi_stale_e2e_secret"}})
	require.ErrorIs(t, err, billing.ErrConflict, "a PSP changed since the revision the edit names")
	require.Equal(t, "revision_mismatch", codeOf(err))
}

// Payments keep working from the cached configuration while Vault does not
// answer.
func TestPaymentsRideOutAVaultOutage(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.vault = vaulttest.New(t)
	target, err := url.Parse(w.vault.Addr)
	require.NoError(t, err)
	var down atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(rw, `{"errors":["Vault is unavailable"]}`, http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(rw, r)
	}))
	t.Cleanup(front.Close)
	w.vaultAddr = front.URL
	w.start()
	warm := enroll(t, w, "nmi", embedded)
	require.True(t, warm.c.entitled(warm.ent))

	down.Store(true)
	rt := engine.Graph(w.rt).Runtime
	_, err = rt.MerchantConfig.Reload(t.Context(), rt.ConfiguredMerchant())
	require.Error(t, err, "the runtime cannot reach Vault")
	e := enroll(t, w, "nmi", embedded)
	require.True(t, e.c.entitled(e.ent), "a new purchase charges through the cached PSP")
	e.toPeriodEnd()
	w.runRenewals()
	require.True(t, e.c.entitled(e.ent), "a renewal charges through the cached PSP")
}
