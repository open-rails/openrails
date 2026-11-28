//go:build integration

package critical_test

import (
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCredits(t *testing.T) {
	t.Run("OR-J22-S01/usage-is-pending-at-once-and-posts-at-the-days-end", func(t *testing.T) {
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
		// A gate never reports the balance in flight: other gates see usage
		// once it settles.
		lease := newLease()
		_, err = w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, InFlight: []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 600}}})
		require.Equal(t, "in_flight[0].limit", *requireCode(t, err, billing.CodeInvalidParam).Param)
		balance(t, w.alpha, customer, 1000, 0, 1000)
		item := numbered(1, billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(int64(450)), Source: "critical", SourceID: "reserved-work"})
		out := settleUsage(t, w.alpha, lease, item...)
		require.EqualValues(t, 1, out.Usage.ThroughN)
		// The 450 is pending, off what the customer may spend at once; the
		// ledger has not moved.
		balance(t, w.alpha, customer, 550, 450, 1000)
		out = settleUsage(t, w.alpha, lease, item...)
		require.EqualValues(t, 1, out.Usage.ThroughN)
		balance(t, w.alpha, customer, 550, 450, 1000)
		// A billing pass within the day posts nothing; the next day's posts it.
		fleet := startFleet(t, w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 550, 450, 1000)
		nextDay(w)
		postDays(t, fleet)
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
	t.Run("OR-J22-S54/a-grant-that-has-already-expired-or-overflows-is-refused", func(t *testing.T) {
		a := newAuthorityWorld(t)
		for i, expiresAt := range []time.Time{a.clock.Now(), a.clock.Now().Add(-time.Hour)} {
			_, err := a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{
				CustomerID: billing.CustomerID(uuid.New()), Currency: "USD", Amount: 1000,
				Source: "critical", SourceID: fmt.Sprintf("expired-%d", i), ExpiresAt: &expiresAt,
			}})
			failure := requireCode(t, err, billing.CodeInvalidParam)
			require.Equal(t, http.StatusBadRequest, failure.Status)
			require.Equal(t, "items[0].expires_at", *failure.Param)
		}

		short := []billing.CreateCreditGrantParams{{
			CustomerID: billing.CustomerID(uuid.New()), Currency: "USD", Amount: 1000,
			Source: "critical", SourceID: "short", ExpiresAt: ptr(a.clock.Now().Add(time.Second)),
		}}
		grants, err := a.alphaRemote.CreateCreditGrants(t.Context(), short)
		require.NoError(t, err)
		require.Len(t, grants, 1)
		a.clock.Advance(2 * time.Second)
		grants, err = a.alphaRemote.CreateCreditGrants(t.Context(), short)
		require.NoError(t, err)
		require.Len(t, grants, 1)
		require.True(t, grants[0].Replayed)

		_, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{
			CustomerID: billing.CustomerID(uuid.New()), Currency: "USD", Amount: math.MaxInt64,
			Source: "critical", SourceID: "overflow",
		}})
		failure := requireCode(t, err, billing.CodeInvalidParam)
		require.Equal(t, http.StatusBadRequest, failure.Status)
		require.Equal(t, "items[0].amount", *failure.Param)
		grants, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{
			CustomerID: billing.CustomerID(uuid.New()), Currency: "USD", Amount: 1000,
			Source: "critical", SourceID: "after-overflow",
		}})
		require.NoError(t, err)
		require.Len(t, grants, 1)
	})
}
