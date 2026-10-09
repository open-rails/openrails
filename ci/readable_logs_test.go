//go:build e2e && integration

package ci_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Logs name money as a person reads it: the lapsed-credit sweep reports what it
// clawed back per currency, never a bare sum of native units.
func TestCreditExpiryLogsReadableAmounts(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "readable-logs-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)

	customer := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	expires := time.Now().UTC().Add(time.Hour)
	_, err = createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{
		Currency: "USD", Amount: 2_500_000, Source: "support", SourceID: uuid.NewString(), ExpiresAt: &expires,
	})
	require.NoError(t, err)

	logs := logtest.NewGlobal()
	t.Cleanup(logs.Reset)
	worker := riverjobs.CreditExpiryWorker{DB: database, Clock: clockwork.NewFakeClockAt(expires.Add(time.Hour))}
	require.NoError(t, worker.Work(t.Context(), &river.Job[riverjobs.CreditExpiryArgs]{}))
	var expired []any
	for _, entry := range logs.AllEntries() {
		if entry.Message == "clawed back lapsed credit-lot remainders" {
			expired = append(expired, entry.Data["expired"])
		}
	}
	require.Contains(t, expired, "2.50 USD")
}
