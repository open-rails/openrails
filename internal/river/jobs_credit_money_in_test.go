package riverjobs

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// When the Charger/Alerter (and/or CreditsService) are not configured, the
// money-in workers must log-and-skip rather than panic or error, so they are
// safe to register before the processor/notification wiring lands.
func TestMoneyInWorkers_NilDepsSkipCleanly(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, LowBalanceAlertWorker{}.Work(ctx, &river.Job[LowBalanceAlertArgs]{}))
	require.NoError(t, AutoTopupWorker{}.Work(ctx, &river.Job[AutoTopupArgs]{}))
	require.NoError(t, ArrearsChargeWorker{}.Work(ctx, &river.Job[ArrearsChargeArgs]{}))
	require.NoError(t, CreditReconcileWorker{}.Work(ctx, &river.Job[CreditReconcileArgs]{}))
}

func TestMoneyInWorkers_Kinds(t *testing.T) {
	require.Equal(t, KindLowBalanceAlert, LowBalanceAlertWorker{}.Kind())
	require.Equal(t, KindAutoTopup, AutoTopupWorker{}.Kind())
	require.Equal(t, KindArrearsCharge, ArrearsChargeWorker{}.Kind())
	require.Equal(t, KindCreditReconcile, CreditReconcileWorker{}.Kind())
}
