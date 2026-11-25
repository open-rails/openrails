//go:build integration

package critical_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// usageOff is the line the server logs when the default ClickHouse is not
// there.
const usageOff = "the default local ClickHouse is unreachable, so usage is off: set CLICKHOUSE_URL to enable it"

// serverClickHouseDefault runs the openrails binary with no clickhouse.url
// and nothing on the default's port: it serves, usage off, and says so once.
// A clickhouse.url the environment names that does not answer keeps it from
// serving.
func serverClickHouseDefault(t *testing.T) {
	if conn, err := net.DialTimeout("tcp", "localhost:9000", time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("something listens on localhost:9000, the default clickhouse.url's port: this scenario needs it free")
	}
	w := newWorld(t)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema + "_auth"}.Sanitize()+" CASCADE")
	})
	keys := t.TempDir()
	writeSigningKeys(t, keys)
	totp := make([]byte, 32)
	_, err := rand.Read(totp)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(base64.StdEncoding.EncodeToString(totp)), 0o600))
	boot := func(env ...string) (string, *lockedBuffer) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		cmd := hostCLICommand(t, w, map[string]any{"port": port, "public_url": "https://billing.critical.invalid", "auth": map[string]any{"keys": map[string]any{"path": keys}}}, "run-server")
		cmd.Env = append(cmd.Env, env...)
		log := &lockedBuffer{}
		cmd.Stdout, cmd.Stderr = log, log
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return fmt.Sprintf("127.0.0.1:%d", port), log
	}
	get := func(url string) (int, []byte) {
		response, err := http.Get(url)
		if err != nil {
			return 0, nil
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, body
	}

	addr, log := boot()
	require.Eventually(t, func() bool {
		status, _ := get("http://" + addr + "/health/ready")
		return status == http.StatusOK
	}, 90*time.Second, 500*time.Millisecond, "without the default ClickHouse the server is ready: %s", log)
	status, raw := get("http://" + addr + "/v1/config")
	require.Equal(t, http.StatusOK, status, string(raw))
	var public billing.PublicConfig
	require.NoError(t, json.Unmarshal(raw, &public))
	require.False(t, public.Usage, "no event store: usage is off")
	require.Equal(t, 1, strings.Count(log.String(), usageOff), "one line says usage is off and how to enable it: %s", log)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	addr, log = boot(fmt.Sprintf("CLICKHOUSE_URL=clickhouse://127.0.0.1:%d/openrails", closed))
	require.Eventually(t, func() bool {
		return strings.Count(log.String(), "clickhouse ping failed; retrying") >= 2
	}, 60*time.Second, 200*time.Millisecond, "a named ClickHouse is waited for: %s", log)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
	}
	require.Error(t, err, "the server does not serve while a named ClickHouse does not answer")
	require.NotContains(t, log.String(), usageOff)
}
