package riverjobs

import (
	"context"
	"errors"
	"testing"

	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

func TestSolanaGasAlertWarnsWhileReturningPartialFailure(t *testing.T) {
	previous := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previous) })
	hook := logtest.NewGlobal()
	failure := errors.New("merchant B RPC unavailable")
	worker := SolanaGasAlertWorker{Check: func(context.Context) ([]solanamodule.CrankerBalance, error) {
		return []solanamodule.CrankerBalance{{Network: "devnet", Lamports: 0}, {Network: "devnet", Lamports: solanamodule.CrankerWarningLamports}}, failure
	}}
	require.ErrorIs(t, worker.Work(t.Context(), nil), failure)
	entries := hook.AllEntries()
	require.Len(t, entries, 1)
	require.Equal(t, log.WarnLevel, entries[0].Level)
	require.Equal(t, "0 SOL", entries[0].Data["balance"])
	require.Equal(t, "devnet", entries[0].Data["network"])
	hook.Reset()
	worker.Check = func(context.Context) ([]solanamodule.CrankerBalance, error) {
		return []solanamodule.CrankerBalance{{Lamports: 1}}, nil
	}
	require.NoError(t, worker.Work(t.Context(), nil), "low balance needs funding, not an immediate failed-job retry")
}
