//go:build integration

package stripeapi

import "net/http"

// SetTestBaseTransport routes every outbound Stripe request through rt (nil
// restores http.DefaultTransport). Integration-build only: e2e tests install a
// host-rewriting RoundTripper here so real Stripe wire shapes can be served by
// an httptest server while the choke-point guard (readonly enforcement +
// Stripe-Version pinning) still runs above it. Not safe for parallel use —
// the integration suite runs serially (-p 1 -parallel 1).
func SetTestBaseTransport(rt http.RoundTripper) {
	testBaseTransport = rt
}
