//go:build integration && browser

package integrationharness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/stretchr/testify/require"
)

// Build web/admin and install tests/browser dependencies before running this
// browser-tagged contract. It serves the actual compiled console and real API.
func TestMerchantNotificationsBrowser(t *testing.T) {
	_, err := os.Stat("../../web/admin/dist/index.html")
	require.NoError(t, err, "build web/admin before running the console browser contract")
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd", WithConsoleAssets(os.DirFS("../../web/admin/dist")), WithConfig(func(cfg *config.Config) {
		cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
		cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}))
	cp := embcp.Get(surface.App())
	actor := h.ensureAPIKeyActor(cp, dbtest.TestMerchantSlug)
	token, _, err := cp.Core().MintAccessToken(ctx, actor, nil)
	require.NoError(t, err)
	input, err := json.Marshal(map[string]string{"url": surface.BaseURL + "/admin/", "token": token, "merchant": dbtest.TestMerchantSlug})
	require.NoError(t, err)
	script, err := filepath.Abs("../../tests/browser/merchant-notifications.mjs")
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "node", script)
	cmd.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(input))
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
}
