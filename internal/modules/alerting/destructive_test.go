package alerting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDestructiveWebhookRetryKeepsIncidentIdentity(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "destructive-hold:episode:destination", r.Header.Get("Idempotency-Key"))
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := &deliverer{http: srv.Client()}
	status, err := d.postOnceWithKey(context.Background(), srv.URL, []byte(`{"title":"held"}`), "destructive-hold:episode:destination")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, err = d.postOnceWithKey(context.Background(), srv.URL, []byte(`{"title":"held"}`), "destructive-hold:episode:destination")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, status)
	require.Equal(t, 2, calls)
}
