// Package gatefixture is the fixture scripts/go-test-gate_test.sh points the
// gate wrapper at. It lives under testdata so the go tool, go vet and
// golangci-lint never pick it up from a `./...` pattern.
package gatefixture

import "testing"

func TestGateFixturePasses(t *testing.T) {}

func TestGateFixturePassesToo(t *testing.T) {}

func TestGateFixtureSkips(t *testing.T) {
	t.Skip("fixture: the wrapper must reject a skip as a pass")
}
