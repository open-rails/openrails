//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

// fullModeConfig is the operating mode every intent Runner in this package must
// state. or#865 made the origin x mode gate fail CLOSED: a Runner with no
// ModeView cannot tell which mode it is in, so it parks every intent instead of
// executing it. A top-up fixture that expects the charge to happen has to say
// "full" out loud — silence is a wiring bug, not a default.
func fullModeConfig() *config.Config {
	return &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
}

// strptr returns a pointer to s — a shared helper for the money integration suite.
func strptr(s string) *string { return &s }

// spendErr collapses SpendCredits' (transaction, error) to just the error, so a
// test that only cares that the spend succeeded can stay a one-liner:
// require.NoError(t, spendErr(svc.SpendCredits(...))). or#892 gave SpendCredits
// a return value; tests asserting on Replayed bind it normally instead.
func spendErr(_ *models.MoneyTransaction, err error) error { return err }
