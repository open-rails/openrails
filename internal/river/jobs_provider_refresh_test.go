package riverjobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerrecovery"
)

// An account refreshed on schedule never trips the recovery gate: its next
// refresh may start a whole stagger late and still has an hour to complete
// before its coverage is stale.
func TestScheduledRefreshCompletesBeforeCoverageIsStale(t *testing.T) {
	latest := providerrecovery.RefreshInterval + defaultRefreshStagger + defaultRefreshSafetyLag
	require.Equal(t, providerrecovery.SafetyLag, defaultRefreshSafetyLag)
	require.GreaterOrEqual(t, providerrecovery.StaleAfter-latest, time.Hour)
}
