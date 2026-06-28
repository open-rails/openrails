package riverjobs

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The Solana operational workers must log-and-skip when their DB/RPC deps are
// not wired, so they are safe to register before the Solana stack lands.
func TestSolanaOperationalWorkers_NilDepsSkipCleanly(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, (&SolanaReconcileWorker{}).Work(ctx, &river.Job[SolanaReconcileArgs]{}))
	require.NoError(t, (&SolanaGasAlertWorker{}).Work(ctx, &river.Job[SolanaGasAlertArgs]{}))
}

func TestSolanaReconcileKind(t *testing.T) {
	require.Equal(t, KindSolanaReconcile, SolanaReconcileArgs{}.Kind())
	require.Equal(t, KindSolanaReconcile, (&SolanaReconcileWorker{}).Kind())
}
