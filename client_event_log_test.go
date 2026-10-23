package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/eventlog"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestEventReaderTransportAndErrors(t *testing.T) {
	mid := billing.MerchantID(uuid.New())
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/app/events", r.URL.Path)
		w.Header().Set(merchant.BoundHeader, mid.String())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after") {
		case "expired":
			w.WriteHeader(410)
			json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Code: billing.CodeEventCursorExpired}})
			return
		case "invalid":
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Code: billing.CodeInvalidCursor}})
			return
		case "denied":
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Code: "forbidden"}})
			return
		}
		require.Equal(t, []string{"a", "b"}, r.URL.Query()["type"])
		json.NewEncoder(w).Encode(billing.EventPage{NextCursor: "next", RetentionSeconds: 777, Events: []billing.Event{{ID: "hev_opaque", MerchantID: mid, Source: "host", Type: "a", Version: 1, OccurredAt: stamp.Add(-time.Hour), RecordedAt: stamp, Data: json.RawMessage(`{"value":9007199254740993}`)}}})
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	local := &Client{inprocess: true, merchantID: mid, baseURL: "http://embedded", client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	remote, err := newRemoteHandle(server.URL, server.Client(), 0, mid, RemoteCredential{APIKey: "test"})
	require.NoError(t, err)
	var first eventlog.Page
	for _, client := range []*Client{local, remote} {
		reader := client.EventReader()
		got, err := reader.ListEvents(t.Context(), eventlog.Query{Types: []string{"b", "a", "a"}})
		require.NoError(t, err)
		if first.NextCursor == "" {
			first = got
		} else {
			require.Equal(t, first, got)
		}
		for _, test := range []struct {
			after string
			want  error
		}{{"expired", eventlog.ErrExpiredCursor}, {"invalid", eventlog.ErrInvalidCursor}, {"denied", billing.ErrDenied}} {
			_, err = reader.ListEvents(t.Context(), eventlog.Query{After: test.after})
			require.ErrorIs(t, err, test.want)
			var status *billing.StatusError
			require.True(t, errors.As(err, &status))
		}
		var saved string
		consumer := eventlog.Consumer{Reader: reader, Subscription: eventlog.Subscription{Source: "openrails", Scope: mid.String(), Name: "projection", Types: []string{"a", "b"}}, Load: func(context.Context, string) (string, error) { return saved, nil }, Commit: func(_ context.Context, _, _ string, p eventlog.Page) error {
			require.Equal(t, first, p)
			saved = p.NextCursor
			return nil
		}}
		_, err = consumer.Poll(t.Context())
		require.NoError(t, err)
		require.Equal(t, "next", saved)
	}
}

func TestListEventsRequest(t *testing.T) {
	client, requests := recordingRemote(t, map[string]string{"/v1/app/events": `{"events":[],"next_cursor":"next","has_more":false,"retention_seconds":7776000}`})
	page, err := client.ListEvents(t.Context(), billing.EventQuery{After: "cursor+/?", Limit: 25, Types: []string{"premium_ended", "payment_failed"}})
	require.NoError(t, err)
	require.NotNil(t, page.Events)
	require.Equal(t, "next", page.NextCursor)
	request := <-requests
	require.Equal(t, "GET", request.method)
	require.Equal(t, "/v1/app/events", request.path)
	require.Equal(t, "after=cursor%2B%2F%3F&limit=25&type=premium_ended&type=payment_failed", request.query)
	require.Nil(t, request.body)
}
