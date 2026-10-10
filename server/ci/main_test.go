//go:build e2e && integration

package ci_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/fxfake"
)

// testFX is the suite's exchange-api: every engine's Deps.FXTransport.
var testFX = fxfake.New()

// The suite fails when an FX request left the process.
func TestMain(m *testing.M) {
	escaped := fxfake.Guard()
	code := m.Run()
	testFX.Close()
	if leaks := escaped(); len(leaks) > 0 {
		fmt.Fprintln(os.Stderr, "FX requests that left the process:\n  "+strings.Join(leaks, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
