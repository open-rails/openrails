//go:build integration

package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func meterForCapture(t *testing.T, w *world) {
	t.Helper()
	product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "compute", DisplayName: "Compute"})
	require.NoError(t, err)
	_, err = w.alpha.SetMeter(t.Context(), "compute", billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum, RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}})})
	require.NoError(t, err)
}

func TestHostTransactions(t *testing.T) {
	for _, scenario := range []struct {
		id     string
		commit bool
	}{{"OR-J23-S04", true}, {"OR-J23-S05", false}} {
		t.Run(scenario.id+"/host-transaction-owns-financial-commit", func(t *testing.T) {
			w := newWorld(t)
			meterForCapture(t, w)
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
			before, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
			require.NoError(t, err)
			tx, err := w.pool.Begin(t.Context())
			require.NoError(t, err)
			defer tx.Rollback(context.Background()) //nolint:errcheck
			_, err = tx.Exec(t.Context(), "SET LOCAL search_path TO "+pgx.Identifier{hostSchema}.Sanitize())
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), "INSERT INTO work(id) VALUES('before-library-call')")
			require.NoError(t, err)
			admitted, err := w.local.AdmitTx(t.Context(), tx, []billing.AdmitParams{{RequestID: "host-work", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 600, ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}})
			require.NoError(t, err)
			require.True(t, admitted[0].Allowed())
			captured, err := w.local.CaptureAdmissionsTx(t.Context(), tx, []billing.CaptureAdmissionParams{{RequestID: "host-work", Amount: ptr(int64(450)), Usage: billing.CaptureUsage{EventType: "compute", Dimensions: map[string]int64{"units": 1}}}})
			require.NoError(t, err)
			require.NoError(t, captured[0].Err())
			require.EqualValues(t, 450, captured[0].Receipt.Amount)
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
			ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
			require.NoError(t, err)
			if scenario.commit {
				require.Equal(t, 2, hostRows)
				balance(t, w.alpha, customer, 550, 0, 550)
				require.Len(t, ledger.Items, 2)
				admission, err := w.alpha.GetAdmission(t.Context(), "host-work")
				require.NoError(t, err)
				require.NotNil(t, admission.State)
				require.Equal(t, billing.AdmissionCaptured, *admission.State)
			} else {
				require.Zero(t, hostRows)
				balance(t, w.alpha, customer, 1000, 0, 1000)
				require.Len(t, ledger.Items, 1)
				_, err := w.alpha.GetAdmission(t.Context(), "host-work")
				require.ErrorIs(t, err, billing.ErrNotFound)
				events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
				require.NoError(t, err)
				require.Empty(t, events.Events)
			}
		})
	}
}
