//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// devOperatorManifest is the operator `task dev` seeds (Taskfile.yaml's
// dev:seed), password and all.
const devOperatorManifest = `users:
  - email: operator@localhost.test
    username: operator
    email_verified: true
    password: { plaintext: "openrails", enforce: true }
    root_role: root:owner
`

// listenHost runs the openrails binary: it listens on loopback unless host
// names another address, and while the operator `task dev` seeds exists it
// refuses to start listening beyond loopback.
func listenHost(t *testing.T) {
	w := newWorld(t)
	t.Cleanup(func() {
		for _, schema := range []string{w.schema + "_auth", w.schema + "_river"} {
			_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	keys := t.TempDir()
	writeSigningKeys(t, keys)
	writeTOTPKey(t, keys)
	settings := func(host string, port int) map[string]any {
		out := map[string]any{"port": port, "public_url": "https://billing.critical.invalid", "auth": map[string]any{"keys": map[string]any{"path": keys}}}
		if host != "" {
			out["host"] = host
		}
		return out
	}
	freePort := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		return port
	}
	withoutHost := func(env []string) []string {
		out := env[:0:0]
		for _, item := range env {
			if !strings.HasPrefix(item, "HOST=") {
				out = append(out, item)
			}
		}
		return out
	}
	// serve starts run-server and waits until it is ready at 127.0.0.1; stop
	// kills it.
	serve := func(host string) (port int, log *lockedBuffer, stop func()) {
		port = freePort()
		cmd := hostCLICommand(t, w, settings(host, port), "run-server")
		cmd.Env = withoutHost(cmd.Env)
		log = &lockedBuffer{}
		cmd.Stdout, cmd.Stderr = log, log
		require.NoError(t, cmd.Start())
		stopped := false
		stop = func() {
			if !stopped {
				stopped = true
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
		t.Cleanup(stop)
		require.Eventually(t, func() bool {
			response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health/ready", port))
			if err != nil {
				return false
			}
			_ = response.Body.Close()
			return response.StatusCode == http.StatusOK
		}, 90*time.Second, 200*time.Millisecond, "the server becomes ready: %s", log)
		return port, log, stop
	}
	// external is an address of this machine off loopback, "" when it has none.
	external := ""
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			external = n.IP.String()
			break
		}
	}
	reachable := func(port int) bool {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(external, fmt.Sprint(port)), time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	}

	// Unset, host is loopback: nothing off this machine reaches the server.
	port, log, stop := serve("")
	require.Contains(t, log.String(), fmt.Sprintf("127.0.0.1:%d", port), "the server names its loopback address: %s", log)
	if external != "" {
		require.False(t, reachable(port), "the default listener is loopback only, not %s", external)
	} else {
		t.Log("no address off loopback on this machine: the default's reach is not probed")
	}
	stop()

	// 0.0.0.0 is every interface.
	port, _, stop = serve("0.0.0.0")
	if external != "" {
		require.True(t, reachable(port), "0.0.0.0 listens on %s too", external)
	}
	stop()

	// The operator `task dev` seeds, with its published password.
	manifest := filepath.Join(t.TempDir(), "bootstrap.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(devOperatorManifest), 0o600))
	seed := hostCLICommand(t, w, settings("", freePort()), "push-auth-bootstrap", "--file", manifest)
	seed.Env = withoutHost(seed.Env)
	out, err := seed.CombinedOutput()
	require.NoError(t, err, "the development operator is seeded: %s", out)

	// Off loopback the server refuses to start while that account exists.
	cmd := hostCLICommand(t, w, settings("0.0.0.0", freePort()), "run-server")
	cmd.Env = withoutHost(cmd.Env)
	refusal := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = refusal, refusal
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		require.Error(t, err, "run-server exits: %s", refusal)
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("run-server neither served nor refused off loopback: %s", refusal)
	}
	require.Contains(t, refusal.String(), "refusing to listen on 0.0.0.0", "it says why: %s", refusal)
	require.Contains(t, refusal.String(), "operator@localhost.test", "it names the account: %s", refusal)
	require.Contains(t, refusal.String(), "Listen on loopback", "it says how to fix it: %s", refusal)
	require.NotContains(t, refusal.String(), "OpenRails serving", "it never listened: %s", refusal)

	// On loopback the development database serves.
	_, _, stop = serve("127.0.0.1")
	stop()
}
