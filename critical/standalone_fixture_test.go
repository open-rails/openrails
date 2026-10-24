//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

type standaloneMerchant struct {
	ID            billing.MerchantID
	Key           string
	CustomerID    billing.CustomerID
	CustomerToken string
	OAuth         iam.OAuthClientCreated
}
type standaloneWorld struct {
	URL, ControlURL                      string
	AliceID, BobID, AliceToken, BobToken string
	Merchants                            map[string]standaloneMerchant
	alpha, beta                          *openrails.Client
}

func newStandaloneWorld(t *testing.T) *standaloneWorld {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	schema := "critical_srv_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, name := range []string{schema + "_auth", schema} {
			_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		}
		pool.Close()
	})
	dir := t.TempDir()
	binary := os.Getenv("OPENRAILS_CRITICAL_SERVER_BINARY")
	if binary == "" {
		binary = filepath.Join(dir, "server-fixture.test")
		build := exec.CommandContext(t.Context(), "go", "test", "-c", "-p", "2", "-o", binary, ".")
		build.Dir = "server-fixture"
		build.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
		output, err := build.CombinedOutput()
		require.NoError(t, err, string(output))
	} else {
		stat, err := os.Stat(binary)
		require.NoError(t, err, "prebuilt standalone fixture must exist")
		require.NotZero(t, stat.Mode()&0111, "prebuilt standalone fixture must be executable")
	}
	ready := filepath.Join(dir, "ready.json")
	fx := fixedFX(t).(loopbackWire)
	data, err := json.Marshal(map[string]any{"Schema": schema, "Ready": ready, "ControlKey": uuid.NewString(), "FXURL": fx.target.String(), "At": time.Now().UTC(), "RedisURL": os.Getenv("OPENRAILS_CRITICAL_REDIS_URL")})
	require.NoError(t, err)
	config := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(config, data, 0600))
	logPath := filepath.Join(dir, "server.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	child := exec.Command(binary, "-test.run=^TestServerFixture$", "-test.timeout=5m", "-test.v")
	child.Env = append(os.Environ(), "OPENRAILS_CRITICAL_SERVER_CONFIG="+config, "GOMAXPROCS=2")
	child.Stdout, child.Stderr = log, log
	require.NoError(t, child.Start())
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = child.Process.Kill()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("owned standalone fixture did not exit")
			}
		}
		_ = log.Close()
		if t.Failed() {
			output, _ := os.ReadFile(logPath)
			t.Logf("owned standalone server: %s", output)
		}
	})
	out := &standaloneWorld{}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			stopped = true
			output, _ := os.ReadFile(logPath)
			t.Fatalf("standalone exited before ready: %v\n%s", err, output)
		case <-deadline.C:
			t.Fatal("standalone did not become ready")
		case <-tick.C:
			if data, err := os.ReadFile(ready); err == nil && json.Unmarshal(data, out) == nil {
				goto ready
			}
		}
	}
ready:
	remote, err := openrails.NewRemote(out.URL, openrails.RemoteOptions{Timeout: 10 * time.Second, Credentials: map[billing.MerchantID]openrails.RemoteCredential{out.Merchants["alpha"].ID: {APIKey: out.Merchants["alpha"].Key}, out.Merchants["beta"].ID: {APIKey: out.Merchants["beta"].Key}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close(context.Background())) })
	out.alpha, err = remote.Merchant(out.Merchants["alpha"].ID)
	require.NoError(t, err)
	out.beta, err = remote.Merchant(out.Merchants["beta"].ID)
	require.NoError(t, err)
	return out
}
func (w *standaloneWorld) request(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), method, w.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}
func (w *standaloneWorld) command(t *testing.T, in any, out any) {
	t.Helper()
	data, err := json.Marshal(in)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.ControlURL, bytes.NewReader(data))
	require.NoError(t, err)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	var envelope struct {
		Result json.RawMessage
		Error  string
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&envelope))
	require.Empty(t, envelope.Error)
	require.NoError(t, json.Unmarshal(envelope.Result, out))
}
func (w *standaloneWorld) challenge(t *testing.T, merchant string) billing.NetworkLinkChallenge {
	t.Helper()
	status, raw := w.request(t, http.MethodPost, "/v1/me/network-link/challenges", w.Merchants[merchant].CustomerToken, map[string]any{})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var challenge billing.NetworkLinkChallenge
	require.NoError(t, json.Unmarshal(raw, &challenge))
	require.NotEmpty(t, challenge.ID)
	require.NotEmpty(t, challenge.Nonce)
	return challenge
}
func (w *standaloneWorld) proof(t *testing.T, merchant, account, nonce string) string {
	t.Helper()
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	w.command(t, map[string]any{"Op": "authorize", "Merchant": merchant, "Account": account, "Nonce": nonce}, &tokens)
	require.NotEmpty(t, tokens.IDToken)
	return tokens.IDToken
}

func (w *standaloneWorld) link(t *testing.T, merchant, account string) billing.CustomerLink {
	t.Helper()
	challenge := w.challenge(t, merchant)
	proof := w.proof(t, merchant, account, challenge.Nonce)
	status, raw := w.request(t, http.MethodPost, "/v1/me/network-link", w.Merchants[merchant].CustomerToken, billing.CreateNetworkLinkParams{ChallengeID: challenge.ID, IDToken: proof})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var link billing.CustomerLink
	require.NoError(t, json.Unmarshal(raw, &link))
	return link
}
