//go:build integration

package embed_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests (offline/sandboxed runs have no Ryuk reaper).
func TestMain(m *testing.M) {
	// pgx decodes timestamptz into time.Local; pin to UTC so cross-transport
	// time comparisons are stable.
	time.Local = time.UTC
	dbtest.RunMain(m)
}
