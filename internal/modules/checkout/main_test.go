//go:build integration

package checkout

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

// fullModeConfig is the operating mode every intent Runner in this package must
// state. or#865 made the origin x mode gate fail CLOSED: a Runner with no
// ModeView cannot tell which mode it is in, so it parks every intent instead of
// executing it. A checkout fixture exercising real provider writes has to say
// "full" out loud — silence is a wiring bug, not a default.
func fullModeConfig() *config.Config {
	return &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
}
