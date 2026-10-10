//go:build e2e && integration

package ci_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/open-rails/openrails/server"
)

// Two standalone servers behind one load balancer share the database, Redis,
// Vault and the signing key: they boot together, run one River fleet, accept each
// other's sign-ins, read each other's writes, and each serves its private
// listener.
func TestServersShareOneDatabase(t *testing.T) {
	f := newFixture(t)
	redis := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if redis == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signing := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	keys, totp := t.TempDir(), make([]byte, 32)
	_, _ = rand.Read(totp)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(hex.EncodeToString(totp)), 0o600))
	vault := vaulttest.New(t)
	shared := func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.Vault = vault.Config()
		cfg.Engine.Redis = &openrails.RedisConfig{Addr: redis}
		cfg.Auth.AllowEphemeralSigningKey = false
		cfg.Auth.ActiveKeyID, cfg.Auth.ActivePrivateKeyPEM, cfg.Auth.KeysPath = "e2e-shared", signing, keys
	}

	servers := make([]*server.Server, 2)
	errs := make([]error, len(servers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range servers {
		wg.Go(func() {
			<-start
			servers[i], errs[i] = f.buildServer(t, shared)
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "server %d boots beside the other", i)
		require.NoError(t, servers[i].Start(t.Context()))
	}
	a, b := servers[0], servers[1]

	var leaders int
	require.Eventually(t, func() bool {
		err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "river_leader"}.Sanitize()+" WHERE expires_at > now()").Scan(&leaders)
		return err == nil && leaders == 1
	}, 30*time.Second, 50*time.Millisecond, "one River leader for both servers' billing and AuthKit jobs")

	// A sign-in on one server is good on the other.
	member, token := newOwner(t, a)
	shop, err := a.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: uniqueName("shared"), DisplayName: "Shared Shop", OwnerUserID: member})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	listed, err := b.ListUserMerchants(t.Context(), r)
	require.NoError(t, err, "the other server verifies the token")
	require.Len(t, listed, 1)
	require.Equal(t, shop.MerchantID, listed[0].ID, "and reads the merchant the first one created")

	for i, srv := range servers {
		w := httptest.NewRecorder()
		srv.PrivateHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, w.Code, "server %d metrics", i)
		require.Contains(t, w.Body.String(), `openrails_dependency_up{dependency="redis",class="optional"} 1`)
		w = httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		require.Equal(t, http.StatusOK, w.Code, "server %d ready: %s", i, w.Body.String())
	}
	require.NoError(t, a.Close(context.Background()))
}
