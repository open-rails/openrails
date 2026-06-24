package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/stretchr/testify/require"
)

func TestProviderRefreshProvidersExcludeSolana(t *testing.T) {
	fetchers := map[reconcile.Provider]reconcile.RailFetcher{
		reconcile.ProviderSolana: providerRefreshFakeFetcher{provider: reconcile.ProviderSolana},
		reconcile.ProviderStripe: providerRefreshFakeFetcher{provider: reconcile.ProviderStripe},
		reconcile.ProviderNMI:    providerRefreshFakeFetcher{provider: reconcile.ProviderNMI},
		reconcile.ProviderCCBill: providerRefreshFakeFetcher{provider: reconcile.ProviderCCBill},
	}

	require.Equal(t, []reconcile.Provider{
		reconcile.ProviderCCBill,
		reconcile.ProviderNMI,
		reconcile.ProviderStripe,
	}, refreshProviders(fetchers))
}

func TestProviderRefreshDefaults(t *testing.T) {
	worker := ProviderRefreshWorker{}
	require.Equal(t, defaultRefreshWindow, worker.window())
	require.Equal(t, defaultRefreshSafetyLag, worker.safetyLag())
	require.Equal(t, defaultRefreshInitialLookback, worker.initialLookback())
	require.Equal(t, defaultRefreshMaxWindows, worker.maxWindows())

	worker.Window = time.Hour
	worker.SafetyLag = time.Minute
	worker.InitialLookback = 2 * time.Hour
	worker.MaxWindows = 3
	require.Equal(t, time.Hour, worker.window())
	require.Equal(t, time.Minute, worker.safetyLag())
	require.Equal(t, 2*time.Hour, worker.initialLookback())
	require.Equal(t, 3, worker.maxWindows())
}

type providerRefreshFakeFetcher struct {
	provider reconcile.Provider
}

func (f providerRefreshFakeFetcher) Name() string { return string(f.provider) }

func (f providerRefreshFakeFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true, Vault: true}
}

func (f providerRefreshFakeFetcher) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return &reconcile.RemoteSnapshot{
		Provider:     f.provider,
		Capabilities: f.Capabilities(),
		Coverage: reconcile.SnapshotCoverage{
			SubscriptionsExhaustive:       true,
			TransactionsExhaustive:        true,
			TransactionsPaginatedComplete: true,
		},
	}, nil
}
