//go:build e2e && integration

package ci_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchants"
)

// A merchant is live or a sandbox, and its PSPs agree with it. One
// deployment serves both side by side: a PSP that disagrees with its merchant
// is refused alone, never failing boot, and the rest of the merchant serves.
// A test seam never sits beside a live PSP.
func TestRealMoneyNeverJoinsTestMoney(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	ctx := t.Context()
	nmi := func(account string) openrails.PSPConfig {
		return openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: account}.PSPConfig()
	}
	stripe := func(account string) openrails.PSPConfig {
		return openrails.StripePSP{Environment: openrails.EnvironmentLive, AccountID: account}.PSPConfig()
	}
	bill, err := openrails.New(ctx, openrails.Config{ProviderWriteMode: openrails.ProviderWritesReadOnly, Merchants: map[string]openrails.MerchantDeclaration{
		"shop":         {DisplayName: "Shop", PSPs: map[string]openrails.PSPConfig{"stripe": stripe("acct_e2e_live"), "nmi": nmi("e2e-live-shop-nmi")}},
		"shop-sandbox": {DisplayName: "Shop Sandbox", Sandbox: true, PSPs: map[string]openrails.PSPConfig{"stripe": stripe("acct_e2e_lab"), "nmi": nmi("e2e-sandbox-nmi")}},
	}}, openrails.Deps{Postgres: pool})
	require.NoError(t, err, "disagreeing PSPs never fail boot")
	t.Cleanup(func() { _ = bill.Close(ctx) })
	refused := probe(t, bill, "openrails_configuration")
	require.ErrorContains(t, refused, "merchants.shop.psps/nmi: psps.nmi.environment: sandbox moves no money, and the merchant is live")
	require.ErrorContains(t, refused, "merchants.shop-sandbox.psps/stripe: psps.stripe.environment: live moves real money, and the merchant is a sandbox (sandbox: true)")
	for slug, served := range map[string]string{"shop": "stripe", "shop-sandbox": "nmi"} {
		m, err := bill.MerchantBySlug(slug)
		require.NoError(t, err)
		psps, err := m.ListPSPs(ctx, billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1, slug)
		require.Equal(t, served, psps.Items[0].Key, "%s serves the PSP that agrees with it", slug)
	}
	require.NoError(t, bill.Close(ctx))

	_, err = openrails.New(ctx, openrails.Config{ProviderWriteMode: openrails.ProviderWritesReadOnly, Merchants: map[string]openrails.MerchantDeclaration{
		"two": {DisplayName: "Two", PSPs: map[string]openrails.PSPConfig{"stripe": stripe("acct_e2e_two")}},
	}}, openrails.Deps{Postgres: pool, FXTransport: testFX.Transport()})
	require.ErrorContains(t, err, "Deps.FXTransport is a test seam and is refused beside a live PSP (two/stripe)")
}

// NMI's own answer must agree with a PSP's declared environment before the PSP
// arms: a test PSP whose gateway account moves real money stays disarmed, and
// the posture probe says why. A test PSP the gateway confirms is armed.
func TestNMIEnvironmentDisagreeingWithTheGatewayIsDisarmed(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	ctx := t.Context()
	gateway := roundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		if form.Get("report_type") != "test_mode_status" {
			t.Errorf("only the read-only test-mode query reaches the gateway, not %s %s", req.Method, req.URL)
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		}
		testMode := "true"
		if form.Get("security_key") == "e2e-moves-money" {
			testMode = "false"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/xml"}},
			Body: io.NopCloser(strings.NewReader("<nm_response><test_mode_enabled>" + testMode + "</test_mode_enabled></nm_response>"))}, nil
	})
	psp := func(account, key string) openrails.PSPConfig {
		return openrails.NMIPSP{Environment: openrails.EnvironmentTest, GatewayID: account, SecurityKey: key, WebhookSigningSecret: "whsec-e2e"}.PSPConfig()
	}
	bill, err := openrails.New(ctx, openrails.Config{
		ProviderWriteMode: openrails.ProviderWritesFull,
		Merchants: map[string]openrails.MerchantDeclaration{"shop": {DisplayName: "Shop", Sandbox: true, PSPs: map[string]openrails.PSPConfig{
			"mobius":   psp("e2e-test-mode", "e2e-test-mode"),
			"paykings": psp("e2e-real-money", "e2e-moves-money"),
		}}},
	}, openrails.Deps{Postgres: pool, FXTransport: testFX.Transport(), NMITransport: gateway})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bill.Close(t.Context()) })

	rt := engine.Graph(bill).Runtime
	disarmed, armed := merchants.PspID("nmi", "test", "e2e-real-money"), merchants.PspID("nmi", "test", "e2e-test-mode")
	require.Eventually(t, func() bool { return rt.PSPPostureDisarmed(disarmed) }, 20*time.Second, 50*time.Millisecond)
	require.False(t, rt.PSPPostureDisarmed(armed), "the gateway confirms the test PSP's environment")
	var probe error
	for _, p := range bill.Probes() {
		if p.Name == "openrails_psp_posture" {
			probe = p.Check(ctx)
		}
	}
	require.ErrorContains(t, probe, "the NMI account is not in the PSP's declared environment")
	require.ErrorContains(t, probe, "it moves real money; the PSP declares test")
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
