//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"
)

// A new browser page has no in-memory access token. The control plane must
// restore its session through the protected cookie used by auth-ui, while
// rejecting body-token fallback and cross-origin cookie consumption.
func TestControlPlaneBrowserSessionUsesRefreshCookie(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, nil)
	user := newAccount(t, cp)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	base := "http://127.0.0.1/" + f.schema + "/v1"
	send := func(method, path, access, origin string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		if access != "" {
			r.Header.Set("Authorization", "Bearer "+access)
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	readSession := func(w *httptest.ResponseRecorder) (string, *http.Cookie) {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var result struct {
			Status string `json:"status"`
			Tokens struct {
				Access  string  `json:"access_token"`
				Refresh *string `json:"refresh_token"`
			} `json:"token_set"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, "complete", result.Status)
		require.NotEmpty(t, result.Tokens.Access)
		require.Nil(t, result.Tokens.Refresh, "the HTTP response cannot expose a refresh credential to JavaScript")
		for _, cookie := range w.Result().Cookies() {
			if cookie.Name == iam.InsecureRefreshCookieName && cookie.Value != "" {
				require.True(t, cookie.HttpOnly)
				require.Equal(t, "/", cookie.Path)
				require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
				require.NotContains(t, w.Body.String(), cookie.Value)
				return result.Tokens.Access, cookie
			}
		}
		t.Fatal("the explicit loopback HTTP deployment must set its protected refresh cookie")
		return "", nil
	}

	login := send(http.MethodPost, "/password/login", "", "http://127.0.0.1", map[string]string{"identifier": user.Email, "password": user.Password})
	_, first := readSession(login)
	refresh := map[string]string{"grant_type": "refresh_token"}
	require.Equal(t, http.StatusForbidden, send(http.MethodPost, "/token", "", "https://foreign.example", refresh, first).Code)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPost, "/token", "", "http://127.0.0.1", map[string]string{"grant_type": "refresh_token", "refresh_token": first.Value}, first).Code)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPost, "/token", "", "http://127.0.0.1", refresh, first, first).Code)
	access, rotated := readSession(send(http.MethodPost, "/token", "", "http://127.0.0.1", refresh, first))
	require.NotEqual(t, first.Value, rotated.Value)
	logout := send(http.MethodDelete, "/logout", access, "http://127.0.0.1", nil, rotated)
	require.Equal(t, http.StatusNoContent, logout.Code, logout.Body.String())
	cleared := false
	for _, cookie := range logout.Result().Cookies() {
		if cookie.Name == rotated.Name && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	require.True(t, cleared, "logout must clear the browser's refresh cookie")
	require.Equal(t, http.StatusUnauthorized, send(http.MethodPost, "/token", "", "http://127.0.0.1", refresh, rotated).Code)
}
