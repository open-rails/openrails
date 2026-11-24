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
// connected account (Stripe-Account) a request acts for. A live key arms on a
// Live verdict, a test key on Simulated.
func PostureKey(secretKey, endpoint, account string) providerposture.Key {
	key := providerposture.Key{Rail: "stripe", AccountID: account, Endpoint: endpoint, Credential: providerposture.Fingerprint(secretKey)}
	if liveSecretKey(secretKey) {
		key.Expect = providerposture.Live
	}
	return key
}

// PostureCheck reads the fixed Stripe API root for its authoritative signals,
// for a live key as for a test one: the declared account's identity (when one
// is declared; another account is Mismatched) and the livemode the same key
// reads, which must be its own mode.
func PostureCheck(transport http.RoundTripper, secretKey, connected, declared string) providerposture.Check {
	return func(ctx context.Context) (providerposture.Verdict, error) {
		live := liveSecretKey(secretKey)
		if !live && !strings.HasPrefix(secretKey, "sk_test_") && !strings.HasPrefix(secretKey, "rk_test_") {
			return providerposture.Unknown, errors.New("stripe key has no live or test-mode prefix")
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
		switch {
		case *balance.Livemode && live:
			return providerposture.Live, nil
		case *balance.Livemode:
			return providerposture.Live, errors.New("stripe account reports livemode=true for a test key")
		case live:
			return providerposture.Simulated, errors.New("stripe account reports livemode=false for a live key")
		}
		return providerposture.Simulated, nil
	}
}

// VerifyPosture verifies a loaded secret key now (startup, credential
// create/rotate) and records the verdict for the transport gate: the key's
// mode, and that it belongs to the declared account (acct_...).
func (f *Factory) VerifyPosture(ctx context.Context, secretKey, declared string) providerposture.Status {
	return providerposture.Process().Verify(ctx, PostureKey(secretKey, APIBase, ""), f.PostureCheckFor(secretKey, declared))
}

// PostureCheckFor returns the startup check for a declared account's key.
func (f *Factory) PostureCheckFor(secretKey, declared string) providerposture.Check {
	if f != nil && f.base != nil {
		return func(context.Context) (providerposture.Verdict, error) {
			if liveSecretKey(secretKey) {
				// An injected transport is a fake provider: it moves no money.
				return providerposture.Simulated, errors.New("stripe live key with an injected transport")
			}
			return providerposture.Simulated, nil
		}
	}
	return PostureCheck(http.DefaultTransport, secretKey, "", declared)
}

// WriteDisarmed is the refusal a write with secretKey would meet at the
// transport, from what is known without asking Stripe: a live key to an
// injected (fake) provider, or a key whose recorded verdict refuses writes. A
// key not checked yet is not refused here; its write's gate checks it.
func (f *Factory) WriteDisarmed(secretKey string) error {
	fixture := f != nil && f.base != nil
	live := liveSecretKey(secretKey)
	switch {
	case live && fixture:
		return fmt.Errorf("%w: stripe live key to a fake provider", providerposture.ErrDisarmed)
	case !fixture:
		if s, known := providerposture.Process().Lookup(PostureKey(secretKey, APIBase, "")); known && s.Verdict != providerposture.Unknown && !s.Armed() {
			return fmt.Errorf("%w: stripe key posture is %s", providerposture.ErrDisarmed, s.Verdict)
		}
	}
	return nil
}

// requirePosture gates a mutation on its key's verdict: a live key's live mode
// and a test key's test mode, proven against the declared account when the
// PSP loaded (VerifyPosture). A write waits for that first verdict, at most
// providerposture.DefaultTimeout, and is refused while it is pending
// (ErrPending, an ErrDisarmed); later re-checks reuse the load's check. A live
// key never reaches a fake provider (an injected transport or a literal
// loopback IP); a test key's mutation there is a fixture's.
func (t *guardTransport) requirePosture(req *http.Request) error {
	secretKey := requestSecret(req)
	if t.fixture || loopbackDestination(req) {
		if liveSecretKey(secretKey) {
			return fmt.Errorf("%w: stripe live key to a fake provider", providerposture.ErrDisarmed)
		}
		return nil
	}
	endpoint := req.URL.Scheme + "://" + req.URL.Host
	account := req.Header.Get("Stripe-Account")
	return providerposture.Process().RequireVerified(req.Context(), PostureKey(secretKey, endpoint, account), providerposture.DefaultTimeout)
}

// loopbackDestination reports a request to a literal loopback IP: a fixture.
func loopbackDestination(req *http.Request) bool {
	ip := net.ParseIP(req.URL.Hostname())
	return ip != nil && ip.IsLoopback()
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
