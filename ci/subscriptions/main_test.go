//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/api"
)

// The suite fails when any response named an error code outside
// billing.ErrorCodes, or under another status than the registry's.
func TestMain(m *testing.M) {
	code := m.Run()
	if violations := api.CodeViolations(); len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "error codes answered outside the registry:\n  "+strings.Join(violations, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
