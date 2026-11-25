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
	ID                                          billing.MerchantID
	Key                                         string
	CustomerID                                  billing.CustomerID
	CustomerToken                               string
	OAuth                                       iam.OAuthClientCreated
	DirectoryKey, DirectoryKeyID, ApplicationID string
}
type standaloneWorld struct {
	URL, ControlURL                      string
	DirectoryURL                         string
	AliceID, BobID, AliceToken, BobToken string
	Merchants                            map[string]standaloneMerchant
	alpha, beta                          *openrails.Client
}

func newStandaloneWorld(t *testing.T, configure ...func(map[string]any)) *standaloneWorld {
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
		build := exec.CommandContext(t.Context(), "go", "test", "-tags", "integration", "-c", "-p", "2", "-o", binary, ".")
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
	settings := map[string]any{"Schema": schema, "Ready": ready, "ControlKey": uuid.NewString(), "FXURL": fx.target.String(), "At": time.Now().UTC(), "RedisURL": os.Getenv("OPENRAILS_CRITICAL_REDIS_URL")}
	for _, change := range configure {
		change(settings)
	}
	data, err := json.Marshal(settings)
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
	status, _, data := w.exchange(t, method, path, token, raw, map[string]string{"Content-Type": "application/json"})
	return status, data
}

// send is a bodiless request with headers, answering the response's.
func (w *standaloneWorld) send(t *testing.T, method, path, token string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	return w.exchange(t, method, path, token, nil, headers)
}

// exchange sends one request, and once more with the server's DPoP nonce
// when a bound token's proof needs one (RFC 9449 §8), as a browser does.
func (w *standaloneWorld) exchange(t *testing.T, method, path, token string, body []byte, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	nonce := ""
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, w.URL+path, reader)
		require.NoError(t, err)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		w.authorize(t, req, token, nonce)
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		require.NoError(t, err)
		if next := response.Header.Get("DPoP-Nonce"); attempt == 0 && response.StatusCode == http.StatusUnauthorized && next != "" && confirmation(token) != "" {
			nonce = next
			continue
		}
		return response.StatusCode, response.Header, data
	}
}

// authorize presents token on req as a browser does: a token the server's
// AuthKit bound to a DPoP key goes with a fresh proof (with nonce, when the
// server asked for one) the fixture makes, as it holds the key; any other
// token as Bearer.
func (w *standaloneWorld) authorize(t *testing.T, req *http.Request, token, nonce string) {
	t.Helper()
	if token == "" {
		return
	}
	if confirmation(token) == "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return
	}
	target := *req.URL
	target.RawQuery = ""
	var headers map[string]string
	w.command(t, map[string]any{"Op": "dpop", "Body": map[string]string{"Method": req.Method, "URL": target.String(), "Token": token, "Nonce": nonce}}, &headers)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
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
