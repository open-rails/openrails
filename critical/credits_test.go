//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCredits(t *testing.T) {
	t.Run("OR-J22-S01/reservation-capture-conserves-native-units", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "funding"}})
		require.NoError(t, err)
		require.Len(t, grants, 1)
		balance(t, w.alpha, customer, 1000, 0, 1000)

		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "compute", DisplayName: "Compute"})
		require.NoError(t, err)
		_, err = w.alpha.SetMeter(t.Context(), "compute", billing.SetMeterParams{
			EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum,
			RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}}),
		})
		require.NoError(t, err)
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{{
			RequestID: "reserved-work", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			ExpiresAt: ptr(w.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(int64(600))},
		}})
		require.NoError(t, err)
		require.Len(t, admitted, 1)
		require.True(t, admitted[0].Allowed(), "%+v", admitted[0])
		balance(t, w.alpha, customer, 400, 600, 1000)
		request := []billing.CaptureAdmissionParams{{RequestID: "reserved-work", Amount: ptr(int64(450)), Usage: billing.CaptureUsage{EventType: "compute", Dimensions: map[string]int64{"units": 1}}}}
		captured, err := w.alpha.CaptureAdmissions(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, captured, 1)
		require.NoError(t, captured[0].Err())
		require.NotNil(t, captured[0].Receipt)
		require.EqualValues(t, 450, captured[0].Receipt.Amount)
		// Capturing releases the unused 150-unit reservation atomically.
		balance(t, w.alpha, customer, 550, 0, 550)
		replayed, err := w.alpha.CaptureAdmissions(t.Context(), request)
		require.NoError(t, err)
		require.NoError(t, replayed[0].Err())
		require.True(t, replayed[0].Receipt.Replayed)
		require.Equal(t, captured[0].Receipt.BalanceTransactionID, replayed[0].Receipt.BalanceTransactionID)
		balance(t, w.alpha, customer, 550, 0, 550)
		transactions, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, transactions.Items, 2)
		amounts := []int64{transactions.Items[0].Amount, transactions.Items[1].Amount}
		require.ElementsMatch(t, []int64{1000, -450}, amounts)
		foreign, err := w.beta.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
	})
}
