//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The host's Authenticator, through a real mount: OpenRails answers missing
// or forged credentials with 401, missing permissions with 403 and outages
// with 503. Staff sessions and personal API keys need the same permission,
// without a recent sign-in. /v1/app takes an application and refuses a person.
func TestSecurityHostAuthRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	member := w.newCustomer()
	product := w.giftProduct("content:refusals")
	grant := map[string]any{"items": []any{map[string]any{"customer_id": member.id, "product_id": product, "hours": 24}}}
	type answer struct {
		status          int
		code, challenge string
		metadata        map[string]any
	}
	send := func(authorization, method, path string, body any) answer {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), method, w.server.URL+mountPrefix+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err = io.ReadAll(res.Body)
		require.NoError(t, err)
		var envelope struct {
			Error struct {
				Code     string         `json:"code"`
				Metadata map[string]any `json:"metadata"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		return answer{res.StatusCode, envelope.Error.Code, res.Header.Get("WWW-Authenticate"), envelope.Error.Metadata}
	}
	bearer := func(token string) string { return "Bearer " + token }
	const access = "/v1/admin/product-access"
	for name, tc := range map[string]struct {
		authorization, method, path string
		status                      int
		code, challenge             string
	}{
		"no token":                 {"", http.MethodGet, "/v1/admin/customers", 401, "authentication_required", "Bearer"},
		"a forged token":           {bearer(w.auth.token(t, "staff") + "x"), http.MethodGet, "/v1/admin/customers", 401, "authentication_required", `Bearer error="invalid_token"`},
		"a permission not held":    {bearer(w.auth.token(t, "reader")), http.MethodPost, access, 403, "permission_required", ""},
		"a customer on the admin":  {bearer(member.token), http.MethodGet, "/v1/admin/customers", 403, "permission_required", ""},
		"a stale person":           {bearer(w.auth.staleToken(t, "staff")), http.MethodPost, access, 201, "", ""},
		"a person's API key":       {bearer(w.auth.apiKeyToken(t, "staff")), http.MethodPost, access, 201, "", ""},
		"a person on /v1/app":      {bearer(w.auth.token(t, "staff")), http.MethodGet, "/v1/app/host-events", 403, "application_required", ""},
		"an application on /v1/me": {bearer(w.auth.hostToken(t)), http.MethodGet, "/v1/me", 403, "invoker_scoped_principal", ""},
	} {
		got := send(tc.authorization, tc.method, tc.path, grant)
		require.Equal(t, tc.status, got.status, "%s: %+v", name, got)
		require.Equal(t, tc.code, got.code, name)
		require.Equal(t, tc.challenge, got.challenge, name)
	}

	got := send(bearer(w.auth.hostToken(t)), http.MethodPost, access, grant)
	require.Equal(t, http.StatusCreated, got.status, "the backend's key passes on its permission alone: %+v", got)
	require.True(t, member.entitled("content:refusals"))
	got = send(bearer(w.auth.hostToken(t)), http.MethodGet, "/v1/app/host-events", nil)
	require.Equal(t, http.StatusOK, got.status, "/v1/app takes the application: %+v", got)

	w.auth.down.Store(true)
	got = send(bearer(w.auth.token(t, "staff")), http.MethodGet, "/v1/admin/customers?ids="+uuid.NewString(), nil)
	w.auth.down.Store(false)
	require.Equal(t, http.StatusServiceUnavailable, got.status, "%+v", got)
	require.Equal(t, "authentication_unavailable", got.code)
	require.Empty(t, got.challenge)
}
