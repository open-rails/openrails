//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// River runs the startup provider refresh only once its leader is elected,
// which under load comes after a scenario has started. Another process still
// holding the leadership makes that election late on purpose: a fault fixture
// still installs its fault only after that refresh and the scans it requests.
func TestFaultFixtureWaitsForLateStartupRefresh(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.river_leader (elected_at, expires_at, leader_id, name)
		VALUES (now(), now() + interval '8 seconds', 'departing-process', 'default')`))
	require.NoError(t, err)
	w.start()
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	var scheduler, merchant, active int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT
		count(*) FILTER (WHERE kind = 'openrails.provider_refresh' AND state = 'completed'),
		count(*) FILTER (WHERE kind = 'openrails.provider_refresh_merchant' AND state = 'completed' AND NOT coalesce((args->>'requested')::boolean, false)),
		count(*) FILTER (WHERE kind IN ('openrails.provider_refresh', 'openrails.provider_refresh_merchant') AND state <> 'completed')
		FROM billing.river_job`)).Scan(&scheduler, &merchant, &active))
	require.Equal(t, []int{1, 1, 0}, []int{scheduler, merchant, active}, "the late startup refresh finished before the fault")
}
