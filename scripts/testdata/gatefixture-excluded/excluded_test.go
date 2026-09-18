//go:build gatefixture_tag

// Package gatefixtureexcluded holds its only test behind a build tag, which is
// the or#1013 shape: without the tag the package compiles no test files at all
// and `go test` exits 0.
package gatefixtureexcluded

import "testing"

func TestGateFixtureTagged(t *testing.T) {}
