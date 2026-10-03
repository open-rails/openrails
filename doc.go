// Package openrails is the OpenRails Go SDK (#338): one Client with two
// constructors.
//
//   - NewRemote(baseURL, opts...) talks to a standalone OpenRails over its
//     service-credential-authenticated /v1/merchant/* routes;
//   - openrails/embed.New(...).Client() runs the engine in-process and returns
//     the same Client wired to an in-process transport (#685): an
//     http.RoundTripper dispatching into the neutral /v1/merchant handler.
//
// Parity is structural: one client implementation, one handler surface. The
// dual-mode conformance test in openrails/embed enforces it end to end.
//
// Request, response and identifier types, errors and codes live in package
// billing (#1121). This package stays dependency-light: it must not link the
// engine (enforced by deps_test.go).
package openrails
