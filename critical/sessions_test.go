//go:build integration

package critical_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"
)

// Every AuthKit sign-in session is bound to its browser's DPoP key (RFC 9449).
// signIn opens one with a key of its own, as a browser does, and present sends
// a token bound to a key the suite holds as DPoP with a fresh proof; any other
// credential goes as Bearer. A token minted later for the same session
// (authtest.StaleSession), or re-signed by a scenario, names the same key and
// goes with it too.

// sessionKeys are the browsers' keys, by thumbprint.
var sessionKeys sync.Map

// signIn signs u in with its password through ak's HTTP surface, proving a
// fresh key, and returns the bound session's tokens.
func signIn(t testing.TB, ak *authkit.Client, u authtest.User) iam.TokenSet {
	t.Helper()
	key := authtest.NewDPoPKey(t)
	sessionKeys.Store(key.Thumbprint(), key)
	identifier := u.Email
	if identifier == "" {
		identifier = u.Username
	}
	body, err := json.Marshal(map[string]string{"identifier": identifier, "password": u.Password})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, ak.APIBase()+"/password/login", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("DPoP", key.Proof(t, http.MethodPost, ak.PublicAPIBase()+"/password/login", "", ""))
	w := httptest.NewRecorder()
	ak.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var result struct {
		Status   string        `json:"status"`
		TokenSet *iam.TokenSet `json:"token_set"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Equal(t, "complete", result.Status, w.Body.String())
	require.NotNil(t, result.TokenSet, w.Body.String())
	// authtest's helpers present it with the key too.
	key.Holds(result.TokenSet.AccessToken)
	return *result.TokenSet
}

// present sets r's credential as a browser sends it to ak's deployment: a
// token bound to a key the suite holds goes as DPoP, with a proof naming ak's
// public origin and r's path; any other as Bearer.
func present(t testing.TB, ak *authkit.Client, r *http.Request, token string) {
	t.Helper()
	key, _ := sessionKeys.Load(confirmation(token))
	base, err := url.Parse(ak.PublicAPIBase())
	if key == nil || err != nil || base.Host == "" {
		r.Header.Set("Authorization", "Bearer "+token)
		return
	}
	r.Header.Set("Authorization", "DPoP "+token)
	r.Header.Set("DPoP", key.(*authtest.DPoPKey).Proof(t, r.Method, base.Scheme+"://"+base.Host+r.URL.EscapedPath(), token, ""))
}

// presenting sends every request with token, as present does.
type presenting struct {
	t     testing.TB
	ak    *authkit.Client
	token string
}

func (p presenting) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	present(p.t, p.ak, r, p.token)
	return http.DefaultTransport.RoundTrip(r)
}

// confirmation is a JWT's cnf.jkt (RFC 9449 §6.1): the thumbprint of the key
// it is bound to; "" for none.
func confirmation(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Cnf struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Cnf.JKT
}
