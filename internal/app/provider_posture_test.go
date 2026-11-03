package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/providerposture"
)

func TestPosturePassRechecksOnlyUnansweredPSPs(t *testing.T) {
	armed, ccbill, silent := uuid.New(), uuid.New(), uuid.New()
	statuses := map[uuid.UUID]providerposture.Status{armed: {Verdict: providerposture.Simulated}}
	status := func(id uuid.UUID) (providerposture.Status, bool) { s, ok := statuses[id]; return s, ok }
	var mu sync.Mutex
	calls := map[uuid.UUID]int{}
	verify := func(id uuid.UUID) bool {
		mu.Lock()
		defer mu.Unlock()
		calls[id]++
		// CCBill answers without a cached verdict; silent never answers.
		return id == ccbill
	}
	known := map[uuid.UUID]bool{}
	ids := []uuid.UUID{armed, ccbill, silent}
	for range 3 {
		require.Equal(t, []uuid.UUID{silent}, posturePass(ids, known, status, verify))
	}
	require.Equal(t, map[uuid.UUID]int{ccbill: 1, silent: 3}, calls, "a settled PSP is checked at most once; an unanswered one every pass")
}

func TestPostureBackoffIsCappedAtFiveMinutes(t *testing.T) {
	require.LessOrEqual(t, postureBackoff(0), time.Second)
	var longest time.Duration
	for range 200 {
		d := postureBackoff(40)
		require.LessOrEqual(t, d, 5*time.Minute)
		longest = max(longest, d)
	}
	require.Greater(t, longest, 30*time.Second, "the cap is the posture cap, not the 30 second default")
}

func TestPostureStateReadsArmingAtOnce(t *testing.T) {
	ctx := context.Background()
	registry := providerposture.Process()
	merchant, psp, untracked := uuid.New(), uuid.New(), uuid.New()
	key := providerposture.Key{Rail: "nmi", MerchantID: merchant, PSPID: psp}
	silent := func(context.Context) (providerposture.Verdict, error) {
		return providerposture.Unknown, errors.New("no answer")
	}
	registry.Verify(ctx, key, silent)
	r := &Runtime{}
	r.postureWait.Store(&postureWait{first: true})
	require.ErrorContains(t, r.postureState(), "first verification pending")
	r.providerPosture.AddPSP(merchant, psp, key, silent)
	r.postureWait.Store(&postureWait{psps: []uuid.UUID{psp, untracked}})
	require.ErrorContains(t, r.postureState(), "2 awaiting a provider answer")

	// A mutation's Require records the provider's answer in the registry.
	registry.Verify(ctx, key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Simulated, nil })
	err := r.postureState()
	require.ErrorContains(t, err, "1 awaiting a provider answer", "a PSP never verified still waits")
	require.NotContains(t, err.Error(), key.String())

	r.postureWait.Store(&postureWait{psps: []uuid.UUID{psp}})
	require.NoError(t, r.postureState())

	registry.Verify(ctx, key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Live, nil })
	err = r.postureState()
	require.Error(t, err, "an answer that does not arm is never read as armed")
	require.NotContains(t, err.Error(), "awaiting")
}

func TestNMISandboxProbeVoidOutlivesThePostureDeadline(t *testing.T) {
	voided := make(chan error, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/payments/auth":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-1","response":"1","response_code":"100","response_text":"SUCCESS"}`))
		case "/payments/probe-1/void":
			_, _ = io.Copy(io.Discard, r.Body) // lets the server see the caller hang up
			time.Sleep(300 * time.Millisecond) // past the posture check's deadline
			voided <- r.Context().Err()
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-1","response":"1","response_code":"100","response_text":"Transaction Void Successful"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gateway.Close)
	client := &nmi.NMIClient{SecurityKey: "sandbox-fixture", Environment: config.EnvironmentSandbox, V5BaseURL: gateway.URL}
	registry := providerposture.NewRegistry(nil)
	registry.Timeout = 100 * time.Millisecond

	started := time.Now()
	status := registry.Verify(t.Context(), client.PostureKey(), client.CheckPosture)
	require.Equal(t, providerposture.Simulated, status.Verdict, status.Err)
	require.Less(t, time.Since(started), 300*time.Millisecond, "the verdict does not wait for the void")
	select {
	case err := <-voided:
		require.NoError(t, err, "the void is not cut short by the posture deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("the probe authorization was never voided")
	}
}

func TestNMIGatewayTestModeAnswersClassify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   providerposture.Verdict
	}{
		{"documented-answer", http.StatusOK, "<nm_response><test_mode_enabled>true</test_mode_enabled></nm_response>", providerposture.Simulated},
		{"gateway-error-is-a-failed-read", http.StatusOK, "<nm_response><error_response>Temporary error</error_response></nm_response>", providerposture.Unknown},
		{"rejected-credentials", http.StatusOK, "<nm_response><error_response>Authentication Failed</error_response></nm_response>", providerposture.Unknown},
		{"extra-element", http.StatusOK, "<nm_response><test_mode_enabled>true</test_mode_enabled><note>x</note></nm_response>", providerposture.Unsupported},
		{"unknown-value", http.StatusOK, "<nm_response><test_mode_enabled>maybe</test_mode_enabled></nm_response>", providerposture.Unsupported},
		{"provider-unavailable", http.StatusServiceUnavailable, "", providerposture.Unknown},
		{"unreadable", http.StatusOK, "<nm_response><test_mode", providerposture.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(gateway.Close)
			client := &nmi.NMIClient{SecurityKey: "test-fixture", Environment: config.EnvironmentTest, QueryURL: gateway.URL}
			verdict, err := client.CheckPosture(t.Context())
			require.Equal(t, tc.want, verdict, err)
			if tc.want == providerposture.Unsupported {
				require.ErrorContains(t, err, "unexpected test_mode_status answer")
			}
			if tc.name == "rejected-credentials" {
				require.ErrorIs(t, err, nmi.ErrCredentialsRejected)
			}
		})
	}
}
