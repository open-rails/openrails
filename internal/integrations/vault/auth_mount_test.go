package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Each method logs in at auth/<AuthMount>/login, or at its own name.
func TestLoginAtTheAuthMount(t *testing.T) {
	jwt := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(jwt, []byte("service-account-jwt\n"), 0o600))
	for _, row := range []struct {
		cfg  Config
		path string
		body map[string]any
	}{
		{Config{AuthMethod: "kubernetes", K8sRole: "openrails", K8sJWTPath: jwt, AuthMount: "openrails-prod/k8s"}, "/v1/auth/openrails-prod/k8s/login", map[string]any{"role": "openrails", "jwt": "service-account-jwt"}},
		{Config{AuthMethod: "kubernetes", K8sRole: "openrails", K8sJWTPath: jwt}, "/v1/auth/kubernetes/login", map[string]any{"role": "openrails", "jwt": "service-account-jwt"}},
		{Config{AuthMethod: "approle", RoleID: "r", SecretID: "s", AuthMount: "/openrails-prod/approle/"}, "/v1/auth/openrails-prod/approle/login", map[string]any{"role_id": "r", "secret_id": "s"}},
		{Config{AuthMethod: "approle", RoleID: "r", SecretID: "s"}, "/v1/auth/approle/login", map[string]any{"role_id": "r", "secret_id": "s"}},
	} {
		var mu sync.Mutex
		logins := map[string]map[string]any{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/login") {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				logins[r.URL.Path] = body
				mu.Unlock()
				_, _ = w.Write([]byte(`{"auth":{"client_token":"t","lease_duration":3600,"renewable":false}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"ttl":3600}}`))
		}))
		ctx, cancel := context.WithCancel(context.Background())
		row.cfg.Address = srv.URL
		_, sup, err := Login(ctx, row.cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool { return sup.AuthState() == nil }, 10*time.Second, 20*time.Millisecond, row.path)
		mu.Lock()
		require.Equal(t, map[string]map[string]any{row.path: row.body}, logins)
		mu.Unlock()
		cancel()
		srv.Close()
	}
}
