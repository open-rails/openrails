//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server"
)

// On a stop, Run fails readiness at once and keeps serving for DrainDelay so
// load balancers drop the instance, then closes the listener and returns.
func TestRunDrainsBeforeItStops(t *testing.T) {
	f := newFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	const drainDelay = 2 * time.Second
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Addr, cfg.DrainDelay, cfg.ShutdownTimeout = addr, drainDelay, 10*time.Second
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ran := make(chan error, 1)
	go func() { ran <- srv.Run(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	health := func(path string) (int, string) {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		var body struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Status + body.Error.Message
	}
	require.Eventually(t, func() bool {
		code, _ := health("/health/ready")
		return code == http.StatusOK
	}, 60*time.Second, 50*time.Millisecond, "ready once River runs")

	stopped := time.Now()
	stop()
	require.Eventually(t, func() bool {
		code, msg := health("/health/ready")
		return code == http.StatusServiceUnavailable && msg == "draining"
	}, time.Second, 10*time.Millisecond, "readiness fails as soon as the stop arrives")
	code, _ := health("/health/live")
	require.Equal(t, http.StatusOK, code, "still serving while draining")
	require.Less(t, time.Since(stopped), drainDelay)

	select {
	case err := <-ran:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	require.GreaterOrEqual(t, time.Since(stopped), drainDelay, "Run served out the drain")
	_, err = net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err, "the listener is closed")
}
