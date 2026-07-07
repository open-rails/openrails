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
		client.V5BaseURL = url
	}
	return client
}

func nmiDeleteIntent() gen.OpenrailsRailIntent {
	subID := uuid.New()
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		IntentType:     TypeNMIDeleteSubscription,
		Rail:           "mobius",
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

func TestNMIDeleteExecuteReadOnlyClientParks(t *testing.T) {
	client := newTestNMIClient(t, "")
	client.ReadOnly = true
	h := NewNMIDeleteHandler(nil, nil, fakeNMIResolver{client: client}, nil)

	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class)
	assert.Contains(t, out.Reason, "read-only")
}

func TestNMIDeleteExecuteMissingClientParks(t *testing.T) {
	h := NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil)
	out := h.Execute(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeParked, out.Class, "missing client is a wiring/credentials problem, not a failure")
	assert.Contains(t, out.Reason, "not armed")
}

func TestNMIDeleteVerifyMissingClientStaysAmbiguous(t *testing.T) {
	h := NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil)
	out := h.Verify(context.Background(), nmiDeleteIntent())
	assert.Equal(t, OutcomeAmbiguous, out.Class, "cannot verify without a client; stay unknown")
}

func TestSubscriptionPresentParsesRecurringReport(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		present bool
		wantErr bool
	}{
		{"present", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"active"}`, true, false},
		{"deletion tombstone (200 + inactive) = deleted", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"inactive"}`, false, false},
		{"absent (404 = deleted at NMI)", http.StatusNotFound, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`, false, false},
		{"error response", http.StatusUnauthorized, `{"type":"authenticationError","error_code":"E_AUTH","message":"invalid security key"}`, false, true},
		{"garbage", http.StatusOK, `not json at all`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/subscriptions/sub-1", r.URL.Path)
				w.WriteHeader(tc.status)
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
