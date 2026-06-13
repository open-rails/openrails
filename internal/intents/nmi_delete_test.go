package intents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func newTestNMIClient(t *testing.T, url string) *nmi.NMIClient {
	t.Helper()
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	if url != "" {
		client.DirectPostURL = url
		client.QueryURL = url
	}
	return client
}

func nmiDeleteIntent() gen.OpenrailsProviderIntent {
	subID := uuid.New()
	return gen.OpenrailsProviderIntent{
		ID:             uuid.New(),
		IntentType:     TypeNMIDeleteSubscription,
		Provider:       "mobius",
		SubscriptionID: &subID,
		Origin:         string(OriginUser),
		Attempts:       1,
		Status:         StatusInFlight,
	}
}

func TestNMIDeleteIdempotencyKeyIsDeterministic(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	key := NMIDeleteIdempotencyKey(id)
	assert.Equal(t, "nmi_delete_subscription:11111111-2222-3333-4444-555555555555", key)
	assert.Equal(t, key, NMIDeleteIdempotencyKey(id), "stable across calls (re-cancels revive the same intent)")
}

// TestNMIDeleteExecuteKillSwitchParks pins the #344 contract: kill switch on
// -> the intent PARKS pending with the reason recorded, before any provider
// or database access (handler has nil DB and no reachable gateway).
func TestNMIDeleteExecuteKillSwitchParks(t *testing.T) {
	client := newTestNMIClient(t, "")
	client.SubscriptionDeletesDisabled = true
	h := NewNMIDeleteHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": client}, nil)

	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class, "kill switch must park, NOT fail")
	assert.Contains(t, out.Reason, "kill switch")
}

func TestNMIDeleteExecuteConfigKillSwitchParks(t *testing.T) {
	client := newTestNMIClient(t, "")
	cfg := &config.Config{FeatureFlags: &config.FeatureFlags{DisableProcessorSubscriptionDeletions: true}}
	h := NewNMIDeleteHandler(nil, cfg, map[string]*nmi.NMIClient{"mobius": client}, nil)

	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class)
	assert.Contains(t, out.Reason, "kill switch")
}

func TestNMIDeleteExecuteReadOnlyClientParks(t *testing.T) {
	client := newTestNMIClient(t, "")
	client.ReadOnly = true
	h := NewNMIDeleteHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": client}, nil)

	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class)
	assert.Contains(t, out.Reason, "read-only")
}

func TestNMIDeleteExecuteMissingClientParks(t *testing.T) {
	h := NewNMIDeleteHandler(nil, nil, map[string]*nmi.NMIClient{}, nil)
	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class, "missing client is a wiring/credentials problem, not a failure")
	assert.Contains(t, out.Reason, "not configured")
}

func TestNMIDeleteVerifyMissingClientStaysAmbiguous(t *testing.T) {
	h := NewNMIDeleteHandler(nil, nil, map[string]*nmi.NMIClient{}, nil)
	out := h.Verify(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeAmbiguous, out.Class, "cannot verify without a client; stay unknown")
}

func TestSubscriptionPresentParsesRecurringReport(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		present bool
		wantErr bool
	}{
		{"present", `<nm_response><subscription><subscription_id>sub-1</subscription_id></subscription></nm_response>`, true, false},
		{"absent (empty report)", `<nm_response></nm_response>`, false, false},
		{"absent (other subscription)", `<nm_response><subscription><subscription_id>other</subscription_id></subscription></nm_response>`, false, false},
		{"error response", `<nm_response><error_response>invalid security key</error_response></nm_response>`, false, true},
		{"garbage", `not xml at all`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			h := &NMIDeleteHandler{}
			present, err := h.subscriptionPresent(newTestNMIClient(t, srv.URL), "sub-1")
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.present, present)
		})
	}
}
