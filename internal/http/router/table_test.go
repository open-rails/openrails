package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A path no route serves, or serves for other methods, answers the error
// envelope rather than ServeMux's text.
func TestTableAnswersUnmatchedRequestsInJSON(t *testing.T) {
	table := &Table{}
	table.Handle("GET /v1/things/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	table.Handle("DELETE /v1/things/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	h := table.Handler()
	serve := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	require.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/v1/things/1").Code)
	for _, tc := range []struct {
		method, path, code string
		status             int
	}{
		{http.MethodGet, "/v1/nothing", "route_not_found", http.StatusNotFound},
		{http.MethodPost, "/v1/things/1", "method_not_allowed", http.StatusMethodNotAllowed},
	} {
		rec := serve(tc.method, tc.path)
		require.Equal(t, tc.status, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		require.Equal(t, tc.code, body.Error.Code)
		if tc.status == http.StatusMethodNotAllowed {
			require.Contains(t, rec.Header().Get("Allow"), http.MethodDelete)
		}
	}
}
