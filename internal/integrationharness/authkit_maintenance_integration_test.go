//go:build integration

package integrationharness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Standalone composes AuthKit's cleanup schedule with billing's workers before
// constructing its one River client. This runs with the normal unprivileged
// runtime role; privileged schema initialization happened separately.
func TestStandaloneAuthKitMaintenanceSharesBillingRiver(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	// The package shares its database across workflows. An earlier lifecycle
	// test may have completed this hour's unique cleanup job; isolate this
	// fixture instead of expecting RunOnStart to defeat hourly deduplication.
	_, err := h.Pool().Exec(ctx, "DELETE FROM public.river_job WHERE kind='authkit_cleanup_expired_auth_state' AND state='completed'")
	require.NoError(t, err)
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
