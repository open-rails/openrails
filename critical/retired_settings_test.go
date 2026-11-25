//go:build integration

package critical_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Settings that left the configuration refuse to boot and name what replaced
// them, whichever source sets them.
func TestRetiredRuntimeSettings(t *testing.T) {
	t.Run("OR-J06-S72/retired-settings-refuse-boot", func(t *testing.T) {
		w := newWorld(t)
		const posture, drain, rebills = "openrails merchant writes", "drains for 5s, then stops within 20s", "openrails merchant rebills hold"
		for _, c := range []struct {
			name  string
			extra map[string]any
			env   string
			args  []string
			says  []string
		}{
			{"key", map[string]any{"provider_write_mode": "full"}, "", []string{"run-server"}, []string{"provider_write_mode / PROVIDER_WRITE_MODE was removed", posture}},
			{"env", nil, "PROVIDER_WRITE_MODE=readonly", []string{"book", "status"}, []string{"provider_write_mode / PROVIDER_WRITE_MODE was removed", posture}},
			{"flag", nil, "", []string{"--provider-write-mode", "readonly", "book", "status"}, []string{"--provider-write-mode was removed", posture}},
			{"drain", map[string]any{"drain_delay": "5s"}, "", []string{"run-server"}, []string{"drain_delay / DRAIN_DELAY was removed", drain}},
			{"shutdown", nil, "SHUTDOWN_TIMEOUT=20s", []string{"book", "status"}, []string{"shutdown_timeout / SHUTDOWN_TIMEOUT was removed", drain}},
			{"admission-hold-key", map[string]any{"engine_admission_hold": true}, "", []string{"run-server"}, []string{"engine_admission_hold / ENGINE_ADMISSION_HOLD was removed", rebills}},
			{"admission-hold-env", nil, "ENGINE_ADMISSION_HOLD=true", []string{"book", "status"}, []string{"engine_admission_hold / ENGINE_ADMISSION_HOLD was removed", rebills}},
		} {
			cmd := hostCLICommand(t, w, c.extra, c.args...)
			if c.env != "" {
				cmd.Env = append(cmd.Env, c.env)
			}
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			require.NoError(t, cmd.Start(), c.name)
			// A server that booted anyway is stopped; the refusal comes first.
			stop := time.AfterFunc(time.Minute, func() { _ = cmd.Process.Kill() })
			err := cmd.Wait()
			stop.Stop()
			require.Error(t, err, c.name)
			for _, says := range c.says {
				require.Contains(t, out.String(), says, c.name)
			}
		}
		require.Contains(t, hostCLI(t, w, "book", "status"), "armed", "without them the same configuration loads")
	})
}
