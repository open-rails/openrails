package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppRoleInitialCredentialAndRotation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		firstOK bool
	}{
		{"rotation after login", true},
		{"recovery after rejected login", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("VAULT_SECRETS_PATH", dir)
			t.Setenv("VAULT_MAX_RETRIES", "0")
			path := filepath.Join(dir, "VAULT_SECRET_ID")
			require.NoError(t, os.WriteFile(path, []byte("old-file-credential\n"), 0o600))

			type request struct {
				secretID string
				accept   chan bool
			}
			requests := make(chan request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth/token/lookup-self" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
					return
				}
				assert.Equal(t, "/v1/auth/approle/login", r.URL.Path)
				var body struct {
					RoleID   string `json:"role_id"`
					SecretID string `json:"secret_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "test-role", body.RoleID)
				attempt := request{secretID: body.SecretID, accept: make(chan bool, 1)}
				select {
				case requests <- attempt:
				case <-r.Context().Done():
					return
				}
				select {
				case accepted := <-attempt.accept:
					if !accepted {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"errors":["invalid role or secret ID"]}`))
						return
					}
					_, _ = w.Write([]byte(`{"auth":{"client_token":"test-token","lease_duration":3600,"renewable":false}}`))
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			nextRequest := func() request {
				t.Helper()
				select {
				case attempt := <-requests:
					return attempt
				case <-time.After(10 * time.Second):
					t.Fatal("Vault login did not arrive")
					return request{}
				}
			}

			client, sup, err := Login(ctx, Config{Address: server.URL, AuthMethod: "approle", RoleID: "test-role", SecretID: "configured-credential"})
			require.NoError(t, err)
			first := nextRequest()
			assert.Equal(t, "configured-credential", first.secretID, "the first attempt honours the caller's configuration")
			require.ErrorIs(t, sup.AuthState(), ErrNotAuthenticated)
			if tc.firstOK {
				first.accept <- true
				require.NoError(t, sup.Wait(ctx, 5*time.Second))
			}
			require.NoError(t, os.WriteFile(path, []byte("rotated-file-credential\n"), 0o600))
			if tc.firstOK {
				sup.NotifyPermissionDenied(&vaultapi.ResponseError{StatusCode: http.StatusForbidden})
			} else {
				first.accept <- false
			}
			second := nextRequest()
			assert.Equal(t, "rotated-file-credential", second.secretID, "later attempts reread the mounted file")
			if !tc.firstOK {
				require.ErrorIs(t, sup.AuthState(), ErrNotAuthenticated)
				assert.NotContains(t, sup.AuthState().Error(), "configured-credential")
				assert.NotContains(t, sup.AuthState().Error(), "rotated-file-credential")
			}
			second.accept <- true
			require.NoError(t, sup.Wait(ctx, 5*time.Second))
			assert.Equal(t, "test-token", client.Token())
		})
	}
}
