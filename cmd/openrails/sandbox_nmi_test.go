package main

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"

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
	cmd.SetArgs([]string{"sandbox", "nmi-gateway", "--listen", "127.0.0.1:0", "--config", t.TempDir() + "/none.yaml",
		"--plan", "premium_new=23.00:30"})
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

	// The catalog reference check reads the seeded plan through the real client.
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "mobius", &config.NMIProviderSettings{SecurityKey: "fake"}, true)
	if err != nil {
		t.Fatal(err)
	}
	client.V5BaseURL = base
	plan, err := client.GetRecurringPlanDetailByID(ctx, "premium_new", "usd")
	if err != nil || !plan.Found || plan.ID != "premium_new" || plan.AmountCents != 2300 || plan.DayFrequency != 30 || plan.Payments == nil || *plan.Payments != 0 {
		t.Fatalf("seeded plan %+v, err %v: want premium_new, 2300 cents every 30 days, open-ended", plan, err)
	}
	if missing, err := client.GetRecurringPlanDetailByID(ctx, "absent", "usd"); err != nil || missing.Found {
		t.Fatalf("absent plan %+v, err %v: want not found", missing, err)
	}
	if err := client.AddRecurringPlan(ctx, "created", "Created", 999, "usd", 7, 0); err != nil {
		t.Fatal(err)
	}
	if created, err := client.GetRecurringPlanDetailByID(ctx, "created", "usd"); err != nil || created.AmountCents != 999 || created.DayFrequency != 7 {
		t.Fatalf("plan created through the provider workflow %+v, err %v", created, err)
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
	for _, args := range [][]string{{"--listen", "0.0.0.0:0"}, {"--listen", "localhost:0"}, {"--listen", ":0"}, {"--plan", "no-terms"}, {"--plan", "p=abc:30"}} {
		cmd := newRootCmd()
		cmd.SetArgs(append([]string{"sandbox", "nmi-gateway"}, args...))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--") {
			t.Fatalf("%v: err %v, want a flag refusal", args, err)
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
