//go:build integration

package critical_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// meterForCapture prices compute dynamically: a request states its amount.
func meterForCapture(t *testing.T, w *world) {
	t.Helper()
	product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "compute", DisplayName: "Compute"})
	require.NoError(t, err)
	_, err = w.alpha.SetMeter(t.Context(), "compute", billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum, RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}})})
	require.NoError(t, err)
}

// computeSpend is one compute request costing amount (meterForCapture).
func computeSpend(customer billing.CustomerID, id string, amount int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(amount)}}
}

// spend settles one compute request of amount through a lease of its own,
// on OpenRails' clock.
func spend(t *testing.T, client *openrails.Client, customer billing.CustomerID, id string, amount int64) *billing.SyncLeasesResult {
	t.Helper()
	return settleUsage(t, client, newLease(), numbered(1, billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
		Currency: "USD", EventType: "compute", Dimensions: map[string]int64{"units": 1}, Amount: ptr(amount), Source: "critical", SourceID: id})...)
}

func TestHostTransactions(t *testing.T) {
	for _, scenario := range []struct {
		id     string
		commit bool
	}{{"OR-J23-S04", true}, {"OR-J23-S05", false}} {
		t.Run(scenario.id+"/host-transaction-owns-financial-commit", func(t *testing.T) {
			w := newWorld(t)
			customer := billing.CustomerID(uuid.New())
			_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "host-funding"}})
			require.NoError(t, err)
			hostSchema := "host_" + w.schema
			_, err = w.pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{hostSchema}.Sanitize()+"; CREATE TABLE "+pgx.Identifier{hostSchema, "work"}.Sanitize()+" (id text PRIMARY KEY)")
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := w.pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{hostSchema}.Sanitize()+" CASCADE")
				require.NoError(t, err)
			})
			before, err := w.alpha.ListEvents(t.Context(), billing.EventListParams{Limit: 100})
			require.NoError(t, err)
			tx, err := w.pool.Begin(t.Context())
			require.NoError(t, err)
			defer tx.Rollback(context.Background()) //nolint:errcheck
			_, err = tx.Exec(t.Context(), "SET LOCAL search_path TO "+pgx.Identifier{hostSchema}.Sanitize())
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), "INSERT INTO work(id) VALUES('before-library-call')")
			require.NoError(t, err)
			started, err := w.local.StartRunningChargeTx(t.Context(), tx, billing.StartRunningChargeParams{Reference: "host-work", CustomerID: customer, Currency: "USD", RatePerHour: 3600})
			require.NoError(t, err)
			require.Equal(t, "host-work", started.Reference)
			// This is the host's own table. Library calls must restore the caller's
			// transaction search path and must never commit it implicitly.
			_, err = tx.Exec(t.Context(), "INSERT INTO work(id) VALUES('after-library-call')")
			require.NoError(t, err)
			if scenario.commit {
				require.NoError(t, tx.Commit(t.Context()))
			} else {
				require.NoError(t, tx.Rollback(t.Context()))
			}
			var hostRows int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{hostSchema, "work"}.Sanitize()).Scan(&hostRows))
			if scenario.commit {
				require.Equal(t, 2, hostRows)
				// The charge committed with the host's work.
				charge, err := w.alpha.GetRunningCharge(t.Context(), "host-work")
				require.NoError(t, err)
				require.Nil(t, charge.StoppedAt)
			} else {
				require.Zero(t, hostRows)
				_, err := w.alpha.GetRunningCharge(t.Context(), "host-work")
				require.ErrorIs(t, err, billing.ErrNotFound)
				events, err := w.alpha.ListEvents(t.Context(), billing.EventListParams{After: before.NextCursor, Limit: 100})
				require.NoError(t, err)
				require.Empty(t, events.Events)
			}
		})
	}
}
