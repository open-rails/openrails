package stripeapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/merchant"
)

// ErrProviderReadOnly is returned for every mutating Stripe request of a
// readonly client or of a merchant whose write posture is readonly. It
// survives *url.Error wrapping, so errors.Is works on the error from
// (*http.Client).Do.
var ErrProviderReadOnly = errors.New("stripe: provider writes are blocked (readonly)")

// ErrKeyRefused is Stripe refusing a read's key (401) or its permission (403).
var ErrKeyRefused = errors.New("stripe refused the key")

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
// rejects mutating methods before the network when readOnly, when the
// request's merchant's write posture is readonly or when their key's posture
// is not proven, and pins the API version on every request.
type guardTransport struct {
	base     http.RoundTripper
	readOnly bool
	postures controls.Reader
	// fixture marks an injected fake transport.
	fixture bool
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mutating := req.Method != http.MethodGet && req.Method != http.MethodHead
	if mutating && (t.readOnly || t.postures == nil) {
		return nil, ErrProviderReadOnly
	}
	if mutating {
		if p := t.postures.Posture(req.Context(), requestMerchant(req)); p.ReadOnly() {
			return nil, fmt.Errorf("%w: %s", ErrProviderReadOnly, p.Reason)
		}
		if err := t.requirePosture(req); err != nil {
			return nil, err
		}
	}
	// Pin the API version on a clone (RoundTripper contract); a caller-set
	// version is kept.
	if req.Header.Get(VersionHeader) == "" {
		req = req.Clone(req.Context())
		req.Header.Set(VersionHeader, APIVersion)
	}
	if mutating {
		req = tagBook(req)
	}
	resp, err := t.transport().RoundTrip(req)
	if err == nil && t.gated(req) {
		if refusal := refused(req, resp); refusal != nil {
			return nil, refusal
		}
	}
	return resp, err
}

func (t *guardTransport) transport() http.RoundTripper {
	if t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}

// ReadOnlyClient returns a Stripe client that blocks every write, for read
// paths (reconciliation, payment-state and liveness reads). A write sneaking
// onto such a path fails loudly and locally.
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
// A nil factory uses the ordinary default transport and writes nothing.
type Factory struct {
	base http.RoundTripper
	// wired: base is a test wire that exempts nothing (NewWiredFactory).
	wired bool
	// postures gates each write on its merchant's write posture; without
	// one every write is refused.
	postures controls.Reader
}

// NewFactory sends Stripe's requests to base when it is set: a fake provider,
// where a test key's writes need no posture and a live key's are refused.
func NewFactory(base http.RoundTripper, postures controls.Reader) *Factory {
	return &Factory{base: base, postures: postures}
}

// NewWiredFactory sends every request to wire and exempts nothing: posture is
// checked through wire and every write is gated as against Stripe (a test
// seam, as nmi.NMIClient.UseTransport is).
func NewWiredFactory(wire http.RoundTripper, postures controls.Reader) *Factory {
	return &Factory{base: wire, wired: true, postures: postures}
}

// Client writes as each request's merchant's write posture allows.
func (f *Factory) Client(timeout time.Duration) *http.Client {
	return f.newClient(false, timeout)
}

func (f *Factory) ReadOnlyClient(timeout time.Duration) *http.Client {
	return f.newClient(true, timeout)
}

// HostRewriteTransport sends every request to target regardless of the
// original host, preserving method, path, query, body and headers: how a
// loopback Stripe (Deps.ProviderEndpoints.StripeAPIURL) is installed under the
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

func (f *Factory) newClient(readOnly bool, timeout time.Duration) *http.Client {
	var base http.RoundTripper
	var postures controls.Reader
	fixture := false
	if f != nil {
		base, fixture, postures = f.base, f.base != nil && !f.wired, f.postures
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &guardTransport{readOnly: readOnly, postures: postures, fixture: fixture, base: base},
	}
}

// taggedPaths are the Stripe endpoints OpenRails creates objects at; it
// creates no products, prices or subscriptions.
var taggedPaths = map[string]bool{"/v1/customers": true, "/v1/invoices": true,
	"/v1/payment_intents": true, "/v1/setup_intents": true, "/v1/refunds": true}

// tagBook adds the book id to the metadata of a request creating an object.
func tagBook(req *http.Request) *http.Request {
	if req.Method != http.MethodPost || !taggedPaths[req.URL.Path] || req.Body == nil {
		return req
	}
	mid, ok := merchant.FromContext(req.Context())
	if !ok {
		return req
	}
	id := booktag.For(mid.UUID()).ID(req.Context())
	if id == "" {
		return req
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return req
	}
	form, err := url.ParseQuery(string(raw))
	key := "metadata[" + booktag.StripeKey + "]"
	if err == nil && form.Get(key) == "" {
		form.Set(key, id)
		raw = []byte(form.Encode())
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	return req
}
