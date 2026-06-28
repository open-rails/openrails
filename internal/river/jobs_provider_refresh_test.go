package riverjobs

import (
	"context"
	"testing"

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
