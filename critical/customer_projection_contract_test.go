//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCustomerProjectionContracts(t *testing.T) {
	t.Run("OR-J07-S05/merchant-customer-reads-survive-hosted-link-storage-transfer", func(t *testing.T) {
		var schema string
		w := newStandaloneWorld(t, func(settings map[string]any) { schema = settings["Schema"].(string) })
		customer := w.Merchants["alpha"].CustomerID
		foreign := w.Merchants["beta"].CustomerID
		_, err := w.beta.UpdateCustomer(t.Context(), foreign, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		settings := billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 5_000_000}}}
		_, err = w.alpha.UpdateCustomer(t.Context(), customer, settings)
		require.NoError(t, err)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{
			CustomerID: customer, Currency: "USD", Amount: 12_000_000,
			Source: "merchant", SourceID: "customer-read-transfer",
		}})
		require.NoError(t, err)
		before, err := w.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err)
		require.Equal(t, customer, before.ID)
		require.Equal(t, settings.CreditLimits, before.Settings.CreditLimits)
		require.Len(t, before.Balances, 1)
		require.Equal(t, "USD", before.Balances[0].Currency)
		require.EqualValues(t, 12_000_000, before.Balances[0].BalanceAmount)

		pool, err := pgxpool.New(t.Context(), os.Getenv("OPENRAILS_CRITICAL_DSN"))
		require.NoError(t, err)
		defer pool.Close()
		var absent bool
		err = pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NULL", schema+".customer_links").Scan(&absent)
		require.NoError(t, err)
		require.True(t, absent, "generic customer reads operate without hosted relationship storage")

		after, err := w.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err, "ordinary billing reads must not depend on platform account-link storage")
		require.Equal(t, before.ID, after.ID)
		require.Equal(t, before.Contact, after.Contact)
		require.Equal(t, before.Settings, after.Settings)
		require.Equal(t, before.DefaultPaymentMethods, after.DefaultPaymentMethods)
		require.Len(t, after.Balances, 1)
		require.EqualValues(t, 12_000_000, after.Balances[0].BalanceAmount)
		page, err := w.alpha.ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{customer, foreign}})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, customer, page.Items[0].ID)
		require.Equal(t, before.Settings, page.Items[0].Settings)
		require.EqualValues(t, 12_000_000, page.Items[0].Balances[0].BalanceAmount)
		_, err = w.beta.GetCustomer(t.Context(), customer)
		require.ErrorIs(t, err, billing.ErrNotFound)
		status, raw := w.request(t, http.MethodGet, "/v1/admin/customers/"+customer.String(), w.Merchants["alpha"].Key, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var response map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &response))
		require.NotContains(t, response, "network", "the hosted projection is absent, not a nullable compatibility field")
	})
}
