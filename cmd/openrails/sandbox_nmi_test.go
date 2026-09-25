package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSandboxNMIGatewayServesTheQualificationProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	// Through the real root: its config pre-run must not apply (no config here).
	cmd := newRootCmd()
	cmd.SetArgs([]string{"sandbox", "nmi-gateway", "--listen", "127.0.0.1:0", "--config", t.TempDir() + "/none.yaml"})
	cmd.SetOut(out)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	var base string
	for deadline := time.Now().Add(10 * time.Second); base == ""; {
		if s := out.String(); strings.HasPrefix(s, "nmi-gateway: ") {
			base = strings.TrimSpace(strings.TrimPrefix(s, "nmi-gateway: "))
		} else if time.Now().After(deadline) {
			t.Fatalf("no listen address announced: %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.PostForm(base+"/api/query.php", url.Values{"report_type": {"test_mode_status"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "<test_mode_status>enabled</test_mode_status>") {
		t.Fatalf("qualification probe answered %d %q", resp.StatusCode, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not stop on cancel")
	}
}

func TestSandboxNMIGatewayRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", ":0"} {
		cmd := newRootCmd()
		cmd.SetArgs([]string{"sandbox", "nmi-gateway", "--listen", addr})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("--listen %s: err %v, want a loopback refusal", addr, err)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
