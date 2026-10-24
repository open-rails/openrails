//go:build integration

package critical_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCreditAtomicity(t *testing.T) {
	t.Run("OR-J22-S04/batch-conflict-rolls-back-ledger-and-events", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		existing := billing.CreateCreditGrantParams{CustomerID: customer, Currency: "USD", Amount: 50, Source: "critical", SourceID: "existing-receipt"}
		initial, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{existing})
		require.NoError(t, err)
		require.Len(t, initial, 1)
		before, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		first := billing.CreateCreditGrantParams{CustomerID: customer, Currency: "USD", Amount: 100, Source: "critical", SourceID: "new-receipt"}
		conflict := existing
		conflict.Amount = 51
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{first, conflict})
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
		balance(t, w.alpha, customer, 50, 0, 50)
		grants, err := w.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, grants.Items, 1)
		require.Equal(t, initial[0].ID, grants.Items[0].ID)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 1)
		require.EqualValues(t, 50, ledger.Items[0].Amount)
		after, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, after.Events)
		// The earlier item's valid receipt identity must have rolled back too.
		corrected, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{first, existing})
		require.NoError(t, err)
		require.Len(t, corrected, 2)
		require.False(t, corrected[0].Replayed)
		require.True(t, corrected[1].Replayed)
		balance(t, w.alpha, customer, 150, 0, 150)
		ledger, err = w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
	})
}
