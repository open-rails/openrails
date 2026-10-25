//go:build integration

package critical_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var hostCLIBuild struct {
	sync.Once
	binary string
	err    error
	output []byte
}

func hostCLI(t *testing.T, w *world, args ...string) string {
	t.Helper()
	binary := os.Getenv("OPENRAILS_CRITICAL_CLI_BINARY")
	if binary == "" {
		hostCLIBuild.Do(func() {
			dir, err := os.MkdirTemp("", "openrails-critical-cli-")
			if err != nil {
				hostCLIBuild.err = err
				return
			}
			hostCLIBuild.binary = filepath.Join(dir, "openrails")
			build := exec.Command("go", "build", "-p", "2", "-o", hostCLIBuild.binary, "./cmd/openrails")
			build.Dir = "../server"
			build.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
			hostCLIBuild.output, hostCLIBuild.err = build.CombinedOutput()
		})
		require.NoError(t, hostCLIBuild.err, string(hostCLIBuild.output))
		binary = hostCLIBuild.binary
	}
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
	require.NoError(t, err, "trusted operator CLI: %s", output)
	return string(output)
}
