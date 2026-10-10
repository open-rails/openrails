// Package stripeapi is the choke point for all outbound Stripe HTTP (there is
// no stripe-go dependency). Every call site takes its *http.Client from here:
// the transport pins Stripe-Version and, in readonly mode, fails every
// non-GET/HEAD request locally with ErrProviderReadOnly.
package stripeapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

// ErrProviderReadOnly is returned for every mutating Stripe request in
// readonly mode. It survives *url.Error wrapping, so errors.Is works on the
// error from (*http.Client).Do.
var ErrProviderReadOnly = errors.New("stripe: provider writes are blocked (mode=readonly)")

// DefaultTimeout is applied when a caller passes timeout <= 0.
const DefaultTimeout = 20 * time.Second

// VersionHeader is Stripe's API-version request header.
const VersionHeader = "Stripe-Version"

// APIVersion is the Stripe API version the hand-written parsers are coded
// against. It is pinned on every outbound request (never the account default)
// and on the registered webhook endpoint, so a Stripe version roll cannot
// change field shapes under us. Bump deliberately, after testing.
const APIVersion = "2026-06-24.dahlia"

// guardTransport is the one transport every Stripe call flows through: it
// rejects mutating methods before the network when readOnly and pins the API
// version on every request.
type guardTransport struct {
	base     http.RoundTripper
	readOnly bool
	// sandbox (test_mode=sandbox) requires every mutation's key to hold a
	// verified test-mode posture; fixture marks an injected fake transport.
	sandbox, fixture bool
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mutating := req.Method != http.MethodGet && req.Method != http.MethodHead
	if t.readOnly && mutating {
		return nil, ErrProviderReadOnly
	}
	if t.sandbox && mutating {
		// An injected transport is not a posture exemption for a live key.
		if t.fixture && liveSecretKey(requestSecret(req)) {
			return nil, fmt.Errorf("%w: stripe live key under sandbox posture", providerposture.ErrDisarmed)
		}
		if !t.fixture {
			if err := requirePosture(req, t.transport()); err != nil {
				return nil, err
			}
		}
	}
	// Pin the API version on a clone (RoundTripper contract); a caller-set
	// version is kept.
	if req.Header.Get(VersionHeader) == "" {
		req = req.Clone(req.Context())
		req.Header.Set(VersionHeader, APIVersion)
	}
	return t.transport().RoundTrip(req)
}

func (t *guardTransport) transport() http.RoundTripper {
	if t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}

// Client returns the *http.Client all Stripe API calls must go through. Writes
// (non-GET/HEAD) fail with ErrProviderReadOnly when
// config.IsProviderReadOnly(cfg); reads always pass. A nil cfg fails closed
// (read-only): a wiring bug must never unblock provider writes. timeout <= 0
// selects DefaultTimeout.
func Client(cfg *config.Config, timeout time.Duration) *http.Client {
	return (*Factory)(nil).Client(cfg, timeout)
}

// ReadOnlyClient returns a Stripe client that blocks writes regardless of
// mode, for read paths with no *config.Config at hand (reconciliation,
// payment-state and liveness reads). A write sneaking onto such a path fails
// loudly and locally.
func ReadOnlyClient(timeout time.Duration) *http.Client {
	return (*Factory)(nil).ReadOnlyClient(timeout)
}

// IdempotencyKeyHeader is Stripe's request-dedup header: retrying a mutating
// request with the same key replays the original outcome instead of repeating
// the side effect (https://stripe.com/docs/api/idempotent_requests).
const IdempotencyKeyHeader = "Idempotency-Key"

// SetIdempotencyKey stamps a mutating Stripe request with an idempotency key.
// Intent-driven provider mutations must stamp the intent's idempotency_key so
// a retry or replay cannot move money twice. Empty keys are ignored.
func SetIdempotencyKey(req *http.Request, key string) {
	if req == nil || key == "" {
		return
	}
	req.Header.Set(IdempotencyKeyHeader, key)
}

// Factory owns the immutable HTTP dependency for one runtime. The injected
// transport is borrowed: closing a runtime never closes a host-owned transport.
// A nil factory uses the ordinary default transport.
type Factory struct{ base http.RoundTripper }

func NewFactory(base http.RoundTripper) *Factory { return &Factory{base: base} }

func (f *Factory) Client(cfg *config.Config, timeout time.Duration) *http.Client {
	return f.newClient(cfg == nil || config.IsProviderReadOnly(cfg), cfg != nil && config.IsTestMode(cfg), timeout)
}

func (f *Factory) ReadOnlyClient(timeout time.Duration) *http.Client {
	return f.newClient(true, false, timeout)
}

// HostRewriteTransport sends every request to target regardless of the
// original host, preserving method, path, query, body and headers: how a
// loopback Stripe (config.ProviderSandbox.StripeAPIURL) is installed under the
// guard.
func HostRewriteTransport(target string) http.RoundTripper {
	return hostRewriteTransport{target: target}
}

type hostRewriteTransport struct{ target string }

func (h hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(h.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host, clone.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func (f *Factory) newClient(readOnly, sandbox bool, timeout time.Duration) *http.Client {
	var base http.RoundTripper
	if f != nil {
		base = f.base
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &guardTransport{readOnly: readOnly, sandbox: sandbox, fixture: base != nil, base: base},
	}
}
