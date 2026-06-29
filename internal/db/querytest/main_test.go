//go:build integration

package querytest

import (
	"fmt"
	"os"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("QUERY_TEST_KEEP_DB") != "" {
		fmt.Fprintln(os.Stderr, "querytest: QUERY_TEST_KEEP_DB set; leaving test database running")
		os.Exit(code)
	}
	dbtest.TerminateShared()
	dbtest.TerminateSharedRedis()
	os.Exit(code)
}
