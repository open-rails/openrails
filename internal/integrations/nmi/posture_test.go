package nmi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/stretchr/testify/require"
)

func TestParseGatewayTestModeStrict(t *testing.T) {
	cases := []struct {
		name, body string
		result     TestModeProbeResult
		wantErr    bool
	}{
		{"true", `<nm_response><test_mode_enabled>true</test_mode_enabled></nm_response>`, ProbeSimulated, false},
		{"false", `<nm_response><test_mode_enabled>false</test_mode_enabled></nm_response>`, ProbeLive, false},
		{"missing", `<nm_response/>`, ProbeIndeterminate, true},
		{"duplicate", `<nm_response><test_mode_enabled>true</test_mode_enabled><test_mode_enabled>true</test_mode_enabled></nm_response>`, ProbeIndeterminate, true},
		{"malformed", `<nm_response><test_mode_enabled>true`, ProbeIndeterminate, true},
		{"error", `<nm_response><error_response>bad</error_response></nm_response>`, ProbeIndeterminate, true},
		{"nested", `<nm_response><x><test_mode_enabled>true</test_mode_enabled></x></nm_response>`, ProbeIndeterminate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGatewayTestMode(tc.body)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.result, got)
		})
	}
}

// fakeGateway serves the regular gateway's read-only test_mode_status query
// and counts every request by kind.
type fakeGateway struct {
	*httptest.Server
	testMode              atomic.Value
	queries, mutations    atomic.Int64
	probeAuths, probeVoid atomic.Int64
}

func newFakeGateway(t *testing.T, testMode string) *fakeGateway {
	g := &fakeGateway{}
	g.testMode.Store(testMode)
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/query":
			g.queries.Add(1)
			require.NoError(t, r.ParseForm())
			require.Equal(t, "test_mode_status", r.Form.Get("report_type"))
			mode := g.testMode.Load().(string)
			if mode == "unavailable" {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`<nm_response><test_mode_enabled>` + mode + `</test_mode_enabled></nm_response>`))
		case r.URL.Path == "/payments/auth":
			g.probeAuths.Add(1)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"PROBE","response_code":"100"}`))
		case r.URL.Path == "/payments/probe-txn/void":
			g.probeVoid.Add(1)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"SUCCESS"}`))
		default:
			g.mutations.Add(1)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"txn","response":"1","response_text":"SUCCESS"}`))
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func gatewayClient(t *testing.T, g *fakeGateway, key, deployment string) *NMIClient {
	client, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: key, EndpointDeployment: deployment}, true)
	require.NoError(t, err)
	client.QueryURL = g.URL + "/query"
	client.V5BaseURL = g.URL
	client.DirectPostURL = g.URL + "/transact"
	return client
}

func TestGatewayPostureVerifiedOnceAcrossMutationsWithoutFinancialProbe(t *testing.T) {
	g := newFakeGateway(t, "true")
	client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointGateway)
	require.True(t, client.VerifyPosture(context.Background()).Armed())
	for i := 0; i < 3; i++ {
		require.NoError(t, client.Void(context.Background(), "txn"))
	}
	// A second client loading the same credential reuses the verdict.
	same := *client
	require.NoError(t, same.Void(context.Background(), "txn"))
	require.EqualValues(t, 1, g.queries.Load())
	require.EqualValues(t, 0, g.probeAuths.Load(), "gateway posture must never send a financial request")
	require.EqualValues(t, 4, g.mutations.Load())
}

func TestGatewayPostureUnseenCredentialVerifiesOnFirstMutationOnly(t *testing.T) {
	g := newFakeGateway(t, "true")
	client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointGateway)
	require.NoError(t, client.Void(context.Background(), "txn"))
	require.NoError(t, client.Void(context.Background(), "txn"))
	require.EqualValues(t, 1, g.queries.Load())
	require.EqualValues(t, 2, g.mutations.Load())
}

func TestGatewayPostureLiveOrUnavailableDisarmsMutationsButNotReads(t *testing.T) {
	for _, mode := range []string{"false", "unavailable", "maybe"} {
		t.Run(mode, func(t *testing.T) {
			g := newFakeGateway(t, mode)
			client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointGateway)
			require.False(t, client.VerifyPosture(context.Background()).Armed(), "verified when loaded")
			err := client.Void(context.Background(), "txn")
			require.ErrorIs(t, err, providerposture.ErrDisarmed)
			require.False(t, IsTransportAmbiguous(err), "a refusal never dispatched")
			require.EqualValues(t, 0, g.mutations.Load())
			require.EqualValues(t, 0, g.probeAuths.Load())
			// Reads still reach the gateway.
			_, _ = client.readGatewayTestMode(context.Background())
			require.EqualValues(t, 2, g.queries.Load())
		})
	}
}

func TestGatewayPostureReverifiesWhenCredentialsReloadOrRotate(t *testing.T) {
	g := newFakeGateway(t, "unavailable")
	client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointGateway)
	require.False(t, client.VerifyPosture(context.Background()).Armed())
	require.ErrorIs(t, client.Void(context.Background(), "txn"), providerposture.ErrDisarmed)

	g.testMode.Store("true")
	require.True(t, client.VerifyPosture(context.Background()).Armed(), "a reload re-verifies")
	require.NoError(t, client.Void(context.Background(), "txn"))

	rotated := gatewayClient(t, g, "rotated-"+uuid.NewString(), config.NMIEndpointGateway)
	rotated.accountMerchantID, rotated.accountPSPID = client.AccountIdentity()
	g.testMode.Store("false")
	require.ErrorIs(t, rotated.Void(context.Background(), "txn"), providerposture.ErrDisarmed, "a rotated key never inherits a verdict")
	require.NoError(t, client.Void(context.Background(), "txn"))
	require.EqualValues(t, 3, g.queries.Load())
}

func TestSandboxDeploymentUsesQualificationProbeOnce(t *testing.T) {
	g := newFakeGateway(t, "true")
	client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointSandbox)
	for i := 0; i < 3; i++ {
		require.NoError(t, client.Void(context.Background(), "txn"))
	}
	require.EqualValues(t, 1, g.probeAuths.Load())
	require.EqualValues(t, 1, g.probeVoid.Load())
	require.EqualValues(t, 0, g.queries.Load())
	require.EqualValues(t, 3, g.mutations.Load())
}

// SEC-33 after a restart: posture now verifies in the background, so a live
// credential this process has not seen yet is checked inline, once, before its
// first mutation. A test-mode account is refused; a live one proceeds and is
// not re-queried per mutation.
func TestLivePostureUnseenCredentialVerifiesInlineBeforeFirstMutation(t *testing.T) {
	live := func(g *fakeGateway) *NMIClient {
		client, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "live-" + uuid.NewString(), EndpointDeployment: config.NMIEndpointGateway}, false)
		require.NoError(t, err)
		client.QueryURL = g.URL + "/query"
		client.V5BaseURL = g.URL
		client.DirectPostURL = g.URL + "/transact"
		return client
	}
	testMode := newFakeGateway(t, "true")
	err := live(testMode).Void(context.Background(), "txn")
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "a test-mode account is never approved blind")
	require.EqualValues(t, 0, testMode.mutations.Load())
	require.EqualValues(t, 1, testMode.queries.Load())

	g := newFakeGateway(t, "false")
	client := live(g)
	for range 3 {
		require.NoError(t, client.Void(context.Background(), "txn"))
	}
	require.EqualValues(t, 1, g.queries.Load(), "verified once, inline")
	require.EqualValues(t, 3, g.mutations.Load())
}

func TestLoopbackFixtureMarkerIsExplicitAndLoopbackOnly(t *testing.T) {
	g := newFakeGateway(t, "unavailable")
	client := gatewayClient(t, g, "key-"+uuid.NewString(), config.NMIEndpointGateway)
	require.ErrorIs(t, client.Void(context.Background(), "txn"), providerposture.ErrDisarmed)
	client.LoopbackFixture = true
	require.NoError(t, client.Void(context.Background(), "txn"))
	client.V5BaseURL = "https://sandbox.nmi.com/api/v5"
	require.ErrorIs(t, client.Void(context.Background(), "txn"), providerposture.ErrDisarmed)
}

func TestProxyPostureBindsCredentialAndDestination(t *testing.T) {
	merchantID, pspID := uuid.New(), uuid.New()
	posture, err := ProxyPostureClient(merchantID, pspID, "proxy-account", &config.NMIProviderSettings{SecurityKey: "proxy-key", EndpointDeployment: config.NMIEndpointGateway}, "", true)
	require.NoError(t, err)
	require.Equal(t, config.NMIEndpointGateway, posture.endpointDeployment)
	require.Equal(t, DefaultDirectPostURL, posture.DirectPostURL, "the declared deployment selects the destination")
	owner, psp := posture.AccountIdentity()
	require.Equal(t, merchantID, owner)
	require.Equal(t, pspID, psp)
	require.ErrorIs(t, posture.RequireArmedFor(context.Background(), SandboxDirectPostURL, "proxy-key"), providerposture.ErrDisarmed)
	require.ErrorIs(t, posture.RequireArmedFor(context.Background(), DefaultDirectPostURL, "other-key"), providerposture.ErrDisarmed)
	sandbox, err := ProxyPostureClient(merchantID, pspID, "proxy-account", &config.NMIProviderSettings{SecurityKey: "proxy-key", EndpointDeployment: config.NMIEndpointSandbox}, "", true)
	require.NoError(t, err)
	require.Equal(t, SandboxDirectPostURL, sandbox.DirectPostURL)
	_, err = ProxyPostureClient(uuid.Nil, pspID, "proxy-account", &config.NMIProviderSettings{SecurityKey: "proxy-key"}, "", true)
	require.Error(t, err, "a proxy credential without its PSP identity is refused")
	var none *NMIClient
	require.ErrorIs(t, none.RequireArmedFor(context.Background(), DefaultDirectPostURL, "proxy-key"), providerposture.ErrDisarmed)
}

// SEC-33: under live posture an NMI account must prove it is live. An account
// left in test mode approves without moving money, so every mutation is
// refused before it is sent; the sandbox endpoint is refused outright.
func TestLivePostureRefusesTestModeAccount(t *testing.T) {
	live := func(g *fakeGateway, deployment string) *NMIClient {
		client, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "live-" + uuid.NewString(), EndpointDeployment: deployment}, false)
		require.NoError(t, err)
		client.QueryURL = g.URL + "/query"
		client.V5BaseURL = g.URL
		client.DirectPostURL = g.URL + "/transact"
		return client
	}
	for _, mode := range []string{"true", "unavailable", "maybe"} {
		t.Run(mode, func(t *testing.T) {
			g := newFakeGateway(t, mode)
			client := live(g, config.NMIEndpointGateway)
			require.False(t, client.VerifyPosture(context.Background()).Armed(), "verified when loaded")
			err := client.Void(context.Background(), "txn")
			require.ErrorIs(t, err, providerposture.ErrDisarmed)
			require.EqualValues(t, 0, g.mutations.Load(), "nothing reaches a test-mode account under live posture")
			require.EqualValues(t, 0, g.probeAuths.Load())
		})
	}
	t.Run("live", func(t *testing.T) {
		g := newFakeGateway(t, "false")
		client := live(g, "")
		require.True(t, client.VerifyPosture(context.Background()).Armed())
		require.NoError(t, client.Void(context.Background(), "txn"))
		require.EqualValues(t, 1, g.queries.Load())
		require.EqualValues(t, 0, g.probeAuths.Load(), "live verification never sends a financial probe")
	})
	t.Run("sandbox_endpoint", func(t *testing.T) {
		_, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "live-" + uuid.NewString(), EndpointDeployment: config.NMIEndpointSandbox}, false)
		require.Error(t, err, "the sandbox endpoint is refused under live posture")
	})
}
