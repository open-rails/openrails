//go:build integration && browser

package integrationharness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// Run after pnpm install and pnpm exec playwright install chromium in
// tests/browser: go test -tags=integration,browser ./internal/integrationharness
// -run '^TestBrowserAuthenticationContract$'. All backends are disposable.
func TestBrowserAuthenticationContract(t *testing.T) {
	h := New(t, context.Background())
	f := newDelegationHTTPFixture(t, h)
	cookie := httptest.NewUnstartedServer(nil)
	t.Cleanup(cookie.Close)
	port := strings.Split(cookie.Listener.Addr().String(), ":")[1]
	origin := "https://merchant.test:" + port
	wrap, err := billingauth.CookieAuthentication(origin)
	require.NoError(t, err)
	var mutations atomic.Int32
	var attached, denied atomic.Int32
	auth := billingauth.AuthenticatorFunc(func(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
		c, err := r.Cookie("session")
		if err != nil || c.Value != "approved" {
			return billingauth.UserContext{}, billingauth.ErrUnauthenticated
		}
		return billingauth.UserContext{UserID: "abdd9f2c-04df-48ce-8a6f-f05843175cc6"}, nil
	})
	mux := http.NewServeMux()
	action := wrap(billingauth.ExplicitCredentials(billingauth.Required(auth)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mutations.Add(1); w.WriteHeader(204) }))))
	mux.HandleFunc("POST /action", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil && c.Value == "approved" {
			attached.Add(1)
		}
		capture := httptest.NewRecorder()
		action.ServeHTTP(capture, r)
		if capture.Code == 403 {
			denied.Add(1)
		}
		for key, values := range capture.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(capture.Code)
		_, _ = w.Write(capture.Body.Bytes())
	})
	mux.HandleFunc("/receipt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"mutations": mutations.Load(), "attached": attached.Load(), "denied": denied.Load()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><title>Cookie test</title>")
	})
	cookie.Config.Handler = mux
	cookie.StartTLS()
	input, err := json.Marshal(map[string]string{"issuer": f.issuer.URL, "resource": f.surface.BaseURL, "email": f.email, "password": f.password, "cookie": origin, "sibling": "https://sibling.merchant.test:" + port, "attacker": "https://attacker.test:" + port})
	require.NoError(t, err)
	script, err := filepath.Abs("../../tests/browser/authentication.mjs")
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "node", script)
	cmd.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(input))
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	require.EqualValues(t, 2, mutations.Load())
}
