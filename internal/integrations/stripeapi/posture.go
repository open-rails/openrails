package stripeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/providerposture"
)

// APIBase is the Stripe REST root every production call site targets.
const APIBase = "https://api.stripe.com"

// PostureKey binds a verdict to the secret key, the API endpoint and the
// connected account (Stripe-Account) a request acts for.
func PostureKey(secretKey, endpoint, account string) providerposture.Key {
	return providerposture.Key{Rail: "stripe", AccountID: account, Endpoint: endpoint, Credential: providerposture.Fingerprint(secretKey)}
}

// PostureCheck reads the fixed Stripe API root and requires its authoritative signals: a test-mode key
// prefix, the declared account's identity (when one is declared) and a live
// API read reporting livemode=false for the same key.
func PostureCheck(transport http.RoundTripper, secretKey, connected, declared string) providerposture.Check {
	return func(ctx context.Context) (providerposture.Verdict, error) {
		switch {
		case strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_"):
			return providerposture.Live, errors.New("stripe live key")
		case !strings.HasPrefix(secretKey, "sk_test_") && !strings.HasPrefix(secretKey, "rk_test_"):
			return providerposture.Unknown, errors.New("stripe key has no test-mode prefix")
		}
		client := &http.Client{Transport: transport, Timeout: DefaultTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		read := func(path string, target any) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+path, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+secretKey)
			req.Header.Set(VersionHeader, APIVersion)
			if connected != "" {
				req.Header.Set("Stripe-Account", connected)
			}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("stripe posture read %s: %w", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("stripe posture read %s: http %d", path, resp.StatusCode)
			}
			return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target)
		}
		if declared != "" {
			var account struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			}
			if err := read("/v1/account", &account); err != nil {
				return providerposture.Unknown, err
			}
			if account.Object != "account" || account.ID != declared {
				return providerposture.Mismatched, errors.New("stripe key belongs to a different account than declared")
			}
		}
		var balance struct {
			Object   string `json:"object"`
			Livemode *bool  `json:"livemode"`
		}
		if err := read("/v1/balance", &balance); err != nil {
			return providerposture.Unknown, err
		}
		if balance.Object != "balance" || balance.Livemode == nil {
			return providerposture.Unknown, errors.New("stripe livemode read: unexpected response")
		}
		if *balance.Livemode {
			return providerposture.Live, errors.New("stripe account reports livemode=true")
		}
		return providerposture.Simulated, nil
	}
}

// VerifyPosture verifies a loaded secret key now (startup, credential
// create/rotate) and records the verdict for the transport gate.
// The verdict is bound to the declared account (acct_...) it was checked for.
func (f *Factory) VerifyPosture(ctx context.Context, secretKey, declared string) providerposture.Status {
	key := PostureKey(secretKey, APIBase, "")
	if f != nil && f.base != nil {
		if liveSecretKey(secretKey) {
			return providerposture.Status{Key: key, Verdict: providerposture.Live, Err: errors.New("stripe live key with an injected transport")}
		}
		return providerposture.Status{Key: key, Verdict: providerposture.Simulated}
	}
	return providerposture.Process().Verify(ctx, key, f.PostureCheckFor(secretKey, declared))
}

// PostureCheckFor returns the startup check for a declared account's key.
func (f *Factory) PostureCheckFor(secretKey, declared string) providerposture.Check {
	if f != nil && f.base != nil {
		return func(context.Context) (providerposture.Verdict, error) {
			if liveSecretKey(secretKey) {
				return providerposture.Live, errors.New("stripe live key with an injected transport")
			}
			return providerposture.Simulated, nil
		}
	}
	return PostureCheck(http.DefaultTransport, secretKey, "", declared)
}

// requirePosture gates one sandbox-posture mutation on its key's verdict.
// Requests to a literal loopback IP are fixtures (code-level test seams): they
// still require a test-mode key prefix but cannot reach Stripe.
func requirePosture(req *http.Request, transport http.RoundTripper) error {
	secretKey := requestSecret(req)
	endpoint := req.URL.Scheme + "://" + req.URL.Host
	account := req.Header.Get("Stripe-Account")
	if ip := net.ParseIP(req.URL.Hostname()); ip != nil && ip.IsLoopback() {
		if liveSecretKey(secretKey) {
			return fmt.Errorf("%w: stripe live key under sandbox posture", providerposture.ErrDisarmed)
		}
		return nil
	}
	return providerposture.Process().Require(req.Context(), PostureKey(secretKey, endpoint, account), PostureCheck(transport, secretKey, account, ""))
}

func requestSecret(req *http.Request) string {
	if user, _, ok := req.BasicAuth(); ok {
		return user
	}
	return strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
}

// liveSecretKey reports a live-mode Stripe secret or restricted key.
func liveSecretKey(secretKey string) bool {
	return strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
}
