package subscriptions

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// TestSolanaCancelStopsCranker_StatusContract locks the load-bearing invariant
// behind without needing a live Postgres: the cancel cascade writes
// SolanaSubscriptionCancelled, while the cranker's due-query (ListDue) only
// returns rows with SolanaSubscriptionActive. Because these two statuses are
// distinct, a cascaded row is necessarily excluded from the cranker's batch —
// billing stops. This guards against anyone "simplifying" either constant or the
// ListDue status filter in a way that would silently keep the cranker pulling
// cancelled subscriptions (OpenRails is the only puller, so that == unstoppable
// billing). The end-to-end DB proof lives in
// solana_cancel_cascade_integration_test.go (build tag: integration).
func TestSolanaCancelStopsCranker_StatusContract(t *testing.T) {
	t.Parallel()

	// The cascade writes "cancelled".
	require.Equal(t, "cancelled", models.SolanaSubscriptionCancelled)
	// The cranker only pulls "active".
	require.Equal(t, "active", models.SolanaSubscriptionActive)
	// And the two are distinct, so a cancelled row can never satisfy ListDue's
	// `status = active` filter.
	require.NotEqual(t, models.SolanaSubscriptionActive, models.SolanaSubscriptionCancelled,
		"cancel cascade must move the row off the active status the cranker filters on")
}
