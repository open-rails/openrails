//go:build integration

package critical_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCredits(t *testing.T) {
	t.Run("OR-J22-S01/usage-in-flight-then-settled-conserves-native-units", func(t *testing.T) {
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
		// A gate has the request in flight, then settles it at 450; a resend
		// of the item answers its receipt and settles nothing more.
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 600}}})
		balance(t, w.alpha, customer, 400, 600, 1000)
		item := numbered(1, billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(int64(450)), Source: "critical", SourceID: "reserved-work"})
		out := settleUsage(t, w.alpha, lease, item...)
		require.EqualValues(t, 1, out.Usage.ThroughN)
		// The 450 is held in the customer's day until it posts.
		balance(t, w.alpha, customer, 550, 450, 1000)
		out = settleUsage(t, w.alpha, lease, item...)
		require.EqualValues(t, 1, out.Usage.ThroughN)
		balance(t, w.alpha, customer, 550, 450, 1000)
		fleet := startFleet(t, w)
		nextMonth(w)
		closeMonths(t, fleet)
		balance(t, w.alpha, customer, 550, 0, 550)
		transactions, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, transactions.Items, 2)
		amounts := []int64{transactions.Items[0].Amount, transactions.Items[1].Amount}
		require.ElementsMatch(t, []int64{1000, -450}, amounts)
		require.NotNil(t, transactions.Items[0].Usage, "the deduction names the day's usage")
		require.EqualValues(t, 450, transactions.Items[0].Usage.Amount)
		foreign, err := w.beta.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
	})
	t.Run("OR-J22-S12/a-staff-member-grants-a-full-batch-once-a-minute", func(t *testing.T) {
		a := newAuthorityWorld(t)
		staff := a.remote(t, a.alpha.MerchantID(), a.aliceToken)
		batch := func(source string, n int) []billing.CreateCreditGrantParams {
			items := make([]billing.CreateCreditGrantParams, n)
			for i := range items {
				items[i] = billing.CreateCreditGrantParams{CustomerID: billing.CustomerID(uuid.New()), Currency: "USD", Amount: 1000, Source: "critical", SourceID: fmt.Sprintf("%s-%d", source, i)}
			}
			return items
		}
		grants, err := staff.CreateCreditGrants(t.Context(), batch("promotion", billing.MaxBatchItems))
		require.NoError(t, err, "a full batch is within a person's limit")
		require.Len(t, grants, billing.MaxBatchItems)
		_, err = staff.CreateCreditGrants(t.Context(), batch("past-the-minute", 1))
		var failure *billing.StatusError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, http.StatusTooManyRequests, failure.Status, "past one batch a minute the person is locked out")
		// The merchant's application acting as itself is not metered.
		grants, err = a.alphaRemote.CreateCreditGrants(t.Context(), batch("backend", billing.MaxBatchItems))
		require.NoError(t, err)
		require.Len(t, grants, billing.MaxBatchItems)
	})
}
