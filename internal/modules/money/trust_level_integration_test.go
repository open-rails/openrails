//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestTrustLevelRemainsHostAssignedAcrossDeposits(t *testing.T) {
	svc, _, payer, currency, ctx := moneyInEnv(t)
	deposit := func() {
		_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: currency, Amount: 1_000_000, Source: "pay"})
		require.NoError(t, err)
	}
	level := func(currency string) string {
		got, err := svc.GetTrustLevel(ctx, payer, currency)
		require.NoError(t, err)
		return got
	}

	deposit()
	require.Empty(t, level(currency), "a deposit grants money without granting trust")
	require.NoError(t, svc.SetTrustLevelOverride(ctx, payer, currency, "vip"))
	deposit()
	require.Equal(t, "vip", level(currency))
	require.Empty(t, level("EUR"), "the host assignment is currency-scoped")
	require.NoError(t, svc.SetTrustLevelOverride(ctx, payer, currency, ""))
	deposit()
	require.Empty(t, level(currency), "clearing the host assignment stays cleared after another deposit")
}
