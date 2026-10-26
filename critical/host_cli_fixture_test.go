//go:build integration

package critical_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func hostCLI(t *testing.T, w *world, args ...string) string {
	t.Helper()
	output, err := hostCLIResult(t, w, args...)
	require.NoError(t, err, "trusted operator CLI: %s", output)
	return output
}

func hostCLIResult(t *testing.T, w *world, args ...string) (string, error) {
	t.Helper()
	binary := os.Getenv("OPENRAILS_CRITICAL_CLI_BINARY")
	require.NotEmpty(t, binary, "build server/cmd/openrails once and set OPENRAILS_CRITICAL_CLI_BINARY")
	info, err := os.Stat(binary)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0111, "operator CLI must be executable")
	config := filepath.Join(t.TempDir(), "config.yaml")
	data, err := json.Marshal(map[string]any{"db": map[string]string{"url": os.Getenv("OPENRAILS_CRITICAL_DSN")}, "database": map[string]string{"schema": w.schema, "river_schema": w.schema}, "provider_write_mode": "readonly"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, data, 0600))
	command := exec.CommandContext(t.Context(), binary, append([]string{"--config", config}, args...)...)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "DB_") || strings.HasPrefix(item, "DATABASE_") || strings.HasPrefix(item, "PROVIDER_WRITE_MODE=") {
			continue
		}
		command.Env = append(command.Env, item)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}
