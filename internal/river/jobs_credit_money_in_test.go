package riverjobs

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// When the Charger/Alerter (and/or CreditsService) are not configured, the
// money-in workers must log-and-skip rather than panic or error, so they are
// safe to register before the rail/notification wiring lands.
func TestMoneyInWorkers_NilDepsSkipCleanly(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, LowBalanceAlertWorker{}.Work(ctx, &river.Job[LowBalanceAlertArgs]{}))
	require.NoError(t, AutoTopupWorker{}.Work(ctx, &river.Job[AutoTopupArgs]{}))
	require.NoError(t, InvoiceWorker{}.Work(ctx, &river.Job[InvoiceArgs]{}))
	require.NoError(t, CreditReconcileWorker{}.Work(ctx, &river.Job[CreditReconcileArgs]{}))
}
