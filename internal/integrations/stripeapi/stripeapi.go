// Package stripeapi is the single wire-level choke point for every outbound
// HTTP call OpenRails makes to the Stripe REST API. OpenRails has no stripe-go
// dependency — Stripe calls are raw HTTP — so the readonly enforcement lives in
// the transport: every Stripe call site MUST obtain its *http.Client from this
// package (never http.DefaultClient or an ad-hoc &http.Client{}), which makes
// the guardTransport the one place mode=readonly is enforced for Stripe,
// mirroring the NMI choke point (nmi.sendDirectRequest -> ErrProviderReadOnly).
//
// Semantics: when read-only, every mutating request (anything but GET/HEAD —
// for Stripe's API every POST/DELETE is a write) fails LOCALLY, before any
// bytes hit the network, with ErrProviderReadOnly. GET/HEAD reads (queries,
// verification, reconciliation, thin-event hydration) pass through untouched.
package stripeapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/config"
)

// ErrProviderReadOnly is returned for every mutating Stripe request when
// provider writes are blocked (mode=readonly). http.Client wraps transport
// errors in *url.Error, which unwraps, so callers and tests can
// errors.Is(err, stripeapi.ErrProviderReadOnly) on the error they get back
// from (*http.Client).Do.
var ErrProviderReadOnly = errors.New("stripe: provider writes are blocked (mode=readonly)")

// DefaultTimeout is applied when a caller passes timeout <= 0.
const DefaultTimeout = 20 * time.Second

// VersionHeader is Stripe's API-version request header.
const VersionHeader = "Stripe-Version"

// APIVersion is the Stripe API version OpenRails' hand-written request/response
// parsers are coded against. We pin it on every outbound request (instead of
// floating on the account's default version) so a Stripe-side version roll can
// not silently change field shapes under us. Bump deliberately, only after
// testing the new version — and pin the webhook endpoint to the SAME version in
// the Stripe dashboard (inbound events don't pass through this client). Latest
// stable Stripe release train as of 2026-06-26 (#587).
const APIVersion = "2026-06-24.dahlia"

// guardTransport rejects mutating HTTP methods before they reach the network
// when readOnly is set (reads always pass), and pins the Stripe API version on
// every outbound request. Both concerns live here because this is the one
// transport every Stripe call flows through.
type guardTransport struct {
	base     http.RoundTripper
	readOnly bool
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.readOnly {
		switch req.Method {
		case http.MethodGet, http.MethodHead:
			// reads stay available in readonly mode
		default:
			return nil, ErrProviderReadOnly
		}
	}
	// Pin the API version (#587). Clone so we don't mutate the caller's request
	// (RoundTripper contract); a caller that set its own version is preserved.
	if req.Header.Get(VersionHeader) == "" {
		req = req.Clone(req.Context())
		req.Header.Set(VersionHeader, APIVersion)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Client returns the *http.Client all Stripe API calls must go through. Writes
// (non-GET/HEAD) are blocked with ErrProviderReadOnly when
// cfg.IsProviderReadOnly() (mode=readonly); reads always pass. A nil cfg is
// treated as not read-only (config validation guarantees a config in real
// boots; nil only occurs in tests of unrelated failure paths, which error on
// the missing secret key before any request is built).
//
// timeout <= 0 selects DefaultTimeout.
func Client(cfg *config.Config, timeout time.Duration) *http.Client {
	return newClient(cfg != nil && cfg.IsProviderReadOnly(), timeout)
}

// ReadOnlyClient returns a Stripe client that blocks writes UNCONDITIONALLY,
// regardless of mode. It is for call paths that are reads by design and have
// no *config.Config at hand (webhook thin-event hydration, tenancy
// balance-check credential verification, reconciliation listers): routing them
// through here keeps every Stripe byte on the choke-point transport and turns
// any future write sneaking onto a read path into a loud local failure.
func ReadOnlyClient(timeout time.Duration) *http.Client {
	return newClient(true, timeout)
}

// IdempotencyKeyHeader is Stripe's request-dedup header: retrying a mutating
// request with the same key replays the original outcome instead of repeating
// the side effect (https://stripe.com/docs/api/idempotent_requests).
const IdempotencyKeyHeader = "Idempotency-Key"

// SetIdempotencyKey stamps a mutating Stripe request with an idempotency key.
// Provider-mutation call sites driven by the intent ledger (#358) MUST stamp
// the intent's idempotency_key so an executor retry/replay of the same intent
// cannot move money twice. Empty keys are ignored.
func SetIdempotencyKey(req *http.Request, key string) {
	if req == nil || key == "" {
		return
	}
	req.Header.Set(IdempotencyKeyHeader, key)
}

func newClient(readOnly bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &guardTransport{readOnly: readOnly},
	}
}
