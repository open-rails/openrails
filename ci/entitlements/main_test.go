//go:build e2e && integration

package entitlements_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/fxfake"
)

// testFX is the suite's exchange-api: every engine's Deps.FXTransport.
var testFX = fxfake.New()

// The suite fails when any response named an error code outside
// billing.ErrorCodes, or under another status than the registry's, or when an
// FX request left the process.
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
	if violations := api.CodeViolations(); len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "error codes answered outside the registry:\n  "+strings.Join(violations, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
