package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientIgnoresEnvironmentProxy(t *testing.T) {
	// net/http caches proxy environment configuration process-wide. Use a fresh
	// test process so an earlier HTTP test cannot hide the configured proxy.
	if os.Getenv("OPENRAILS_HTTP_PROXY_TEST_CHILD") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClientIgnoresEnvironmentProxy$", "-test.v")
		cmd.Env = append(os.Environ(), "OPENRAILS_HTTP_PROXY_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("proxy regression: %v\n%s", err, output)
		}
		return
	}

	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "proxy answered without checking the destination")
	}))
	defer proxy.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, proxy.URL)
	}
	for _, key := range []string{"NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(key, "")
	}

	policy := Policy{Allow: AllowLoopback} // Permit the test proxy's socket only.
	client := policy.Client(time.Second)
	defer client.CloseIdleConnections()
	for _, scheme := range []string{"http", "https"} {
		target := scheme + "://outbound-policy.invalid/private"
		if err := policy.ValidateURL(target); err != nil {
			t.Fatal(err)
		}
		resp, err := client.Get(target)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Errorf("unresolvable destination unexpectedly succeeded through %s proxy", scheme)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("environment proxy received %d requests without destination validation", got)
	}
}
