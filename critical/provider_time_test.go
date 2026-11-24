//go:build integration

package critical_test

import (
	"net/http"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// Provider times are read as each provider states them.
func TestProviderTime(t *testing.T) {
	t.Run("OR-J28-S09/an-nmi-nonce-signs-a-delivery-and-its-event-applies-once", func(t *testing.T) {
		a, _, sub := nmiMembership(t)
		queue := hostFleet(t, a.world)
		converges := func() []*rivertype.JobRow {
			t.Helper()
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.subscription_converge"))
			require.NoError(t, err)
			return jobs.Jobs
		}
		deliver := func(id, nonce string) {
			t.Helper()
			status, raw := nmiSignedNotice(t, a, id, *sub.RailSubscriptionID, nonce)
			require.Equal(t, http.StatusOK, status, raw)
		}
		// NMI's t is a nonce, not a time: an opaque one signs a delivery.
		deliver("evt_nonce", "5f2b9c0e7a1d4c83")
		woken := converges()
		require.Len(t, woken, 1)
		_, err := queue.JobDelete(t.Context(), woken[0].ID)
		require.NoError(t, err)
		// The delivery replayed, and NMI's retry of it under a nonce that reads
		// as a long-past time, apply nothing: its event_id was applied.
		deliver("evt_nonce", "5f2b9c0e7a1d4c83")
		deliver("evt_nonce", "1000000000")
		require.Empty(t, converges())
		deliver("evt_after", "1000000000")
		require.Len(t, converges(), 1, "another event under that nonce applies")
	})
}
