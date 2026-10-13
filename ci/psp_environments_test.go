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
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchants"
)

// A deployment's PSPs are all live or all non-live. New refuses declarations
// that mix them before migrating; once a database holds non-live PSPs, a live
// one declared at a later boot is refused; and a test seam never sits beside a
// live PSP.
func TestPSPEnvironmentsNeverMix(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	ctx := t.Context()
	sandboxNMI := openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, AccountID: "e2e-sandbox-nmi"}.PSPConfig()
	liveStripe := openrails.StripePSP{Environment: openrails.EnvironmentLive, AccountID: "acct_e2e_live"}.PSPConfig()
	declare := func(psps map[string]map[string]openrails.PSPConfig) openrails.Config {
		cfg := openrails.Config{ProviderWriteMode: openrails.ProviderWritesReadOnly, Merchants: map[string]openrails.MerchantDeclaration{}}
		for slug, declared := range psps {
			cfg.Merchants[slug] = openrails.MerchantDeclaration{DisplayName: slug, PSPs: declared}
		}
		return cfg
	}

	_, err := openrails.New(ctx, declare(map[string]map[string]openrails.PSPConfig{
		"one": {"nmi": sandboxNMI}, "two": {"stripe": liveStripe},
	}), openrails.Deps{Postgres: pool})
	require.EqualError(t, err, "openrails: Config.Merchants: a deployment is all live or all non-live: live PSPs two/stripe beside non-live PSPs one/nmi (sandbox)")
	var found *string
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('billing.merchants')::text").Scan(&found))
	require.Nil(t, found, "refused before migrating")

	_, err = openrails.New(ctx, declare(map[string]map[string]openrails.PSPConfig{"two": {"stripe": liveStripe}}),
		openrails.Deps{Postgres: pool, FXTransport: testFX.Transport()})
	require.ErrorContains(t, err, "Deps.FXTransport is a test seam and is refused beside a live PSP (two/stripe)")

	bill, err := openrails.New(ctx, declare(map[string]map[string]openrails.PSPConfig{"one": {"nmi": sandboxNMI}}), openrails.Deps{Postgres: pool, FXTransport: testFX.Transport()})
	require.NoError(t, err)
	require.NoError(t, bill.Close(ctx))

	_, err = openrails.New(ctx, declare(map[string]map[string]openrails.PSPConfig{"two": {"stripe": liveStripe}}), openrails.Deps{Postgres: pool})
	require.ErrorContains(t, err, "psps/stripe: a deployment's PSPs are all live or all non-live: one's PSP nmi (nmi e2e-sandbox-nmi) is sandbox, this one live")
	var live int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM billing.psps WHERE environment = 'live'").Scan(&live))
	require.Zero(t, live, "the refused PSP is never registered")
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
		return openrails.NMIPSP{Environment: openrails.EnvironmentTest, AccountID: account, SecurityKey: key, WebhookSigningSecret: "whsec-e2e"}.PSPConfig()
	}
	bill, err := openrails.New(ctx, openrails.Config{
		ProviderWriteMode: openrails.ProviderWritesFull,
		Merchants: map[string]openrails.MerchantDeclaration{"shop": {DisplayName: "Shop", PSPs: map[string]openrails.PSPConfig{
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
