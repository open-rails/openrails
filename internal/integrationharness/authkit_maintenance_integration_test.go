//go:build integration

package integrationharness

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Each standalone AuthKit lifecycle runs its ordinary fleet in one isolated
// database, without draining provider work left by unrelated billing scenarios.
func TestStandaloneAuthKitLifecycle(t *testing.T) {
	if runStandaloneInIsolatedProcess(t) {
		return
	}
	// Check cleanup before the recovery fleets can complete this hour's unique
	// maintenance job. Each subtest closes its fleet before the next starts.
	t.Run("maintenance", testStandaloneAuthKitMaintenance)
	t.Run("api_only", func(t *testing.T) { testStandaloneAccountRecovery(t, false) })
	t.Run("with_workers", func(t *testing.T) { testStandaloneAccountRecovery(t, true) })
}

// dbtest owns one database per process. Reuse the exact test binary so these
// ordinary fleets cannot drain another workflow's queued provider work.
func runStandaloneInIsolatedProcess(t *testing.T) bool {
	t.Helper()
	const childEnv = "OPENRAILS_ISOLATED_STANDALONE_TEST"
	if os.Getenv(childEnv) == t.Name() {
		return false
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	if deadline, ok := t.Deadline(); ok {
		cmd.Args = append(cmd.Args, "-test.timeout="+time.Until(deadline).String())
	}
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err, "isolated standalone workflow")
	require.Contains(t, string(output), "\n--- PASS: "+t.Name()+" (", "isolated workflow must execute and pass")
	require.NotContains(t, string(output), "--- SKIP:", "isolated workflow must execute every scenario")
	return true
}

// Standalone composes AuthKit's cleanup schedule with billing's workers before
// constructing its one River client. This runs with the normal unprivileged
// runtime role; privileged schema initialization happened separately.
func testStandaloneAuthKitMaintenance(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	var eventID int64
	require.NoError(t, h.Pool().QueryRow(ctx, `
 INSERT INTO profiles.session_events(occurred_at,issuer,user_id,session_id,event)
 VALUES(now()-interval '2 years','maintenance-test','user','session','login') RETURNING id`).Scan(&eventID))
	surface := h.StartStandalone("usd", WithWorkers())
	require.NotNil(t, surface.App().Runtime.RiverClient)
	require.False(t, surface.App().Runtime.HasExternalRiverClient())
	require.Eventually(t, func() bool {
		var exists bool
		err := h.Pool().QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM profiles.session_events WHERE id=$1)", eventID).Scan(&exists)
		return err == nil && !exists
	}, 20*time.Second, 50*time.Millisecond, "the standalone fleet must run AuthKit cleanup")
	require.Eventually(t, func() bool {
		var count int
		err := h.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE kind='authkit_cleanup_expired_auth_state' AND queue='authkit_maintenance_profiles' AND state='completed'`).Scan(&count)
		return err == nil && count > 0
	}, 10*time.Second, 25*time.Millisecond)
}
