package nmi

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTokenCurrencyNeverReachesNMI(t *testing.T) {
	provider := newNMIFake(t, reply(approvedDirect))
	client := provider.client(t)
	for _, currency := range []string{"SOL", "USDC"} {
		_, err := client.RunSale(t.Context(), SaleParams{CustomerVaultID: "vault", Amount: 1_000_000, Currency: currency, StoredCredential: citOneTime()})
		require.ErrorContains(t, err, "not supported by card payment rails")
		_, err = client.AddRecurringSubscription(t.Context(), RecurringPaymentData{CustomerVaultID: "vault", PlanID: "plan", Currency: currency, ScheduleOnly: true})
		require.ErrorContains(t, err, "not supported by card payment rails")
		_, err = client.Refund(t.Context(), RefundParams{TransactionID: "old", Amount: 1_000_000, Currency: currency})
		require.ErrorContains(t, err, "not supported by card payment rails")
	}
	require.Empty(t, provider.Calls(), "neither charges, schedules nor refunds may reinterpret token units as card money")
}
