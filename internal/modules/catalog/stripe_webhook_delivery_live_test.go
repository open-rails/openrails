//go:build stripelive

// End-to-end live delivery test for #590: register a managed webhook endpoint
// pointing at a real cloudflared quick-tunnel, trigger a real Stripe event, and
// prove (a) the event is actually delivered to our receiver through the tunnel and
// (b) its signature verifies with the secret OpenRails auto-captured at create —
// using the same sigverify.VerifyStripe the production handler uses.
//
//	export $(grep -E '^STRIPE_SECRET_KEY=' .env | xargs)
//	go test -tags=stripelive ./internal/modules/catalog/ -run TestLiveWebhookDelivery -v -timeout 5m
package catalog

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/shared/sigverify"
	"github.com/stretchr/testify/require"
)

type receivedWebhook struct {
	body []byte
	sig  string
}

func TestLiveWebhookDeliveryThroughTunnel(t *testing.T) {
	svc := liveStripeSvc(t)
	ctx := context.Background()

	if _, err := exec.LookPath("cloudflared"); err != nil {
		t.Skip("cloudflared not installed; skipping live delivery test")
	}

	// 1. Local receiver. /healthz is for the readiness probe (does not enqueue);
	//    /webhooks/stripe records real deliveries.
	recvCh := make(chan receivedWebhook, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/webhooks/stripe", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recvCh <- receivedWebhook{body: body, sig: r.Header.Get("Stripe-Signature")}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 2. Quick cloudflared tunnel -> our local receiver.
	tctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(tctx, "cloudflared", "tunnel", "--url", srv.URL, "--no-autoupdate")
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { cancel(); _ = cmd.Wait() }()

	tunnelURL := waitForTunnelURL(t, stderr, 45*time.Second)
	t.Logf("cloudflared tunnel: %s", tunnelURL)
	waitTunnelReady(t, tunnelURL+"/healthz", 45*time.Second)

	// 3. Register a managed endpoint at the tunnel (captures the signing secret).
	events := []string{"product.created"}
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: tunnelURL + "/webhooks/stripe", EnabledEvents: events})
	require.NoError(t, err)
	require.Equal(t, WebhookCreated, res.Action)
	require.NotEmpty(t, res.Secret)
	secret := res.Secret
	t.Cleanup(func() { _ = svc.DeleteWebhookEndpoint(context.Background(), res.EndpointID) })

	// 4. Trigger a REAL event: create a product -> product.created fires.
	prodID, err := svc.CreateProduct(ctx, CreateProductParams{
		Name:           "openrails-livetest delivery probe",
		IdempotencyKey: "openrails-livetest-delivery-" + res.EndpointID,
	})
	if err != nil {
		t.Skipf("cannot create a product with this key (need products:write): %v", err)
	}
	t.Logf("created product %s to trigger product.created", prodID)
	t.Cleanup(func() {
		inactive := false
		_ = svc.UpdateProduct(context.Background(), prodID, UpdateProductParams{Active: &inactive})
	})

	// 5. Wait for delivery; verify EVERY received event's signature with the
	//    captured secret, and confirm our product.created arrives.
	deadline := time.After(120 * time.Second)
	for {
		select {
		case got := <-recvCh:
			require.NoError(t, sigverify.VerifyStripe(secret, got.sig, got.body, 5*time.Minute),
				"delivered webhook signature must verify with the auto-captured secret")
			var evt struct {
				Type string `json:"type"`
				Data struct {
					Object struct {
						ID string `json:"id"`
					} `json:"object"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(got.body, &evt))
			t.Logf("delivered + signature-verified: %s (%s)", evt.Type, evt.Data.Object.ID)
			if evt.Type == "product.created" && evt.Data.Object.ID == prodID {
				return // success: real delivery + verified signature for our event
			}
		case <-deadline:
			t.Fatal("did not receive a signature-verified product.created for our product within 120s")
		}
	}
}

func waitForTunnelURL(t *testing.T, r io.Reader, timeout time.Duration) string {
	t.Helper()
	re := regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if m := re.FindString(sc.Text()); m != "" {
				urlCh <- m
				return
			}
		}
	}()
	select {
	case u := <-urlCh:
		return u
	case <-time.After(timeout):
		t.Skip("cloudflared did not produce a tunnel URL in time; skipping live delivery test")
		return ""
	}
}

func waitTunnelReady(t *testing.T, healthURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	t.Skip("cloudflared tunnel did not become reachable in time; skipping live delivery test")
}
