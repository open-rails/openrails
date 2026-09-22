//go:build integration && browser

package integrationharness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/stretchr/testify/require"
)

func TestAdminConsoleAccountRecoveryBrowser(t *testing.T) {
	_, err := os.Stat("../../web/admin/dist/index.html")
	require.NoError(t, err, "build web/admin before the browser contract")
	h := New(t, t.Context())
	surface := h.StartStandalone("USD", WithWorkers(), WithConsoleAssets(os.DirFS("../../web/admin/dist")), WithConfig(func(cfg *config.Config) {
		cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
	}))
	cp := embcp.Get(surface.App())
	name := "console-recovery-" + uuid.NewString()[:8]
	password := "Correct-console-recovery-password-1"
	user, err := cp.Core().CreateUser(t.Context(), name+"@example.test", name)
	require.NoError(t, err)
	require.NoError(t, cp.Core().AdminSetPassword(t.Context(), user.ID, password))
	deleted, err := cp.Core().SoftDeleteUsers(t.Context(), []string{user.ID})
	require.NoError(t, err)
	require.NoError(t, deleted[0].Err)
	input, err := json.Marshal(map[string]string{"url": surface.BaseURL, "email": name + "@example.test", "password": password})
	require.NoError(t, err)
	script, err := filepath.Abs("../../tests/browser/admin-recovery.mjs")
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "node", script)
	cmd.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(input))
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	restored, err := cp.Core().AdminGetUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.Nil(t, restored.DeletedAt)
}
