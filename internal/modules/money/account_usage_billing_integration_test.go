//go:build integration

package money

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

type usageTestConfig struct{ settings billing.MerchantSettings }

func (c *usageTestConfig) MerchantSettings(context.Context, billing.MerchantID) (string, billing.MerchantSettings, error) {
	return "Usage", c.settings, nil
}

func TestAccountUsageBillingIntegration(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	ctx := t.Context()
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pc.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "usage_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	riverSchema := schema + "_jobs"
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE; DROP SCHEMA "+pgx.Identifier{riverSchema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema}))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	require.NoError(t, err)
	database.SetRiverJobInserter(jobs)
	cfg := &usageTestConfig{settings: billing.MerchantSettings{UsageBilling: &billing.UsageBillingSettings{BillingDay: 13, Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 100_000_000, MinimumInvoiceAmount: 5_000_000}}}}}
	database.SetMerchantConfig(cfg)
	mid := billing.MerchantID(uuid.New())
	ctx = merchant.WithID(ctx, mid)
	require.NoError(t, database.GenDirectory().InsertMerchant(ctx, gen.InsertMerchantParams{ID: mid.UUID(), Slug: "usage", GroupID: uuid.NewString()}))
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	service := NewMoneyService(database, clock)
	service.UsageBillingEnabled = true
	customer := func() identity.CustomerID {
		id := identity.CustomerID(uuid.New())
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			return database.Gen(ctx).EnsureCustomerRow(ctx, gen.EnsureCustomerRowParams{ID: id.UUID(), MerchantID: mid.UUID()})
		}))
		return id
	}
	accrue := func(id identity.CustomerID, amount int64) {
		_, err := service.AccrueOwed(ctx, id, "USD", "test", uuid.NewString(), amount)
		require.NoError(t, err)
	}
	count := func(id identity.CustomerID) (int, int64) {
		var n int
		var total int64
		err := database.DataPool().QueryRow(ctx, `SELECT count(*),coalesce(sum(total),0)::bigint FROM billing.orders WHERE merchant_id=$1 AND customer_id=$2 AND origin='usage'`, mid.UUID(), id.UUID()).Scan(&n, &total)
		require.NoError(t, err)
		return n, total
	}
	settings := func(id identity.CustomerID) gen.BillingMoneySetting {
		var row gen.BillingMoneySetting
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			row, err = database.Gen(ctx).GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "USD"})
			return err
		}))
		return row
	}
	t.Run("threshold does not move monthly date", func(t *testing.T) {
		id := customer()
		accrue(id, 100_000_000)
		original := settings(id).UsageNextBillAt
		n, err := service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		require.Equal(t, 1, n)
		orders, total := count(id)
		require.Equal(t, 1, orders)
		require.EqualValues(t, 100_000_000, total)
		require.Equal(t, original, settings(id).UsageNextBillAt)
		_, err = service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		orders, _ = count(id)
		require.Equal(t, 1, orders)
	})
	t.Run("minimum carries across months", func(t *testing.T) {
		clock.Advance(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).Sub(clock.Now()))
		id := customer()
		accrue(id, 4_000_000)
		clock.Advance(24 * time.Hour)
		_, err := service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		orders, _ := count(id)
		require.Zero(t, orders)
		require.True(t, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC).Equal(*settings(id).UsageNextBillAt))
		accrue(id, 2_000_000)
		_, err = service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		orders, _ = count(id)
		require.Zero(t, orders)
		clock.Advance(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC).Sub(clock.Now()))
		_, err = service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		orders, total := count(id)
		require.Equal(t, 1, orders)
		require.EqualValues(t, 6_000_000, total)
	})
	t.Run("preemptive collection wakes below the cap", func(t *testing.T) {
		id := customer()
		accrue(id, 98_000_000)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return service.WithDatabase(database.NewWithPgxTx(tx)).RequestUsageCollection(ctx, gen.New(tx), id, "USD")
		}))
		require.True(t, settings(id).UsageCollectionDue)
		_, err := service.FinalizeDueUsage(ctx, clock.Now())
		require.NoError(t, err)
		orders, total := count(id)
		require.Equal(t, 1, orders)
		require.EqualValues(t, 98_000_000, total)
	})
	t.Run("missing default card denies only credit", func(t *testing.T) {
		id := customer()
		capacity, err := service.GetAdmissionCapacity(ctx, id, "USD")
		require.NoError(t, err)
		require.Zero(t, capacity.CreditLimit)
		require.Equal(t, BillingModePrepaid, capacity.BillingMode)
		policy, err := service.GetAccountSettings(ctx, id, "USD")
		require.NoError(t, err)
		require.EqualValues(t, 100_000_000, policy.CreditLimitAmount, "missing card changes usable credit, not configured policy")
		// Facts remain recordable even without admission or a payment method.
		accrue(id, 6_000_000)
	})
	t.Run("parallel date and threshold checks bill once", func(t *testing.T) {
		id := customer()
		accrue(id, 100_000_000)
		errors := make(chan error, 4)
		start := make(chan struct{})
		for range 4 {
			go func() { <-start; _, err := service.FinalizeDueUsage(ctx, clock.Now()); errors <- err }()
		}
		close(start)
		for range 4 {
			require.NoError(t, <-errors)
		}
		orders, total := count(id)
		require.Equal(t, 1, orders)
		require.EqualValues(t, 100_000_000, total)
	})
	t.Run("one malformed profile cannot starve later customers", func(t *testing.T) {
		bad, good := customer(), customer()
		if bad.String() > good.String() {
			bad, good = good, bad
		}
		accrue(bad, 100_000_000)
		accrue(good, 100_000_000)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return gen.New(tx).UpsertCustomerInvoiceProfile(ctx, gen.UpsertCustomerInvoiceProfileParams{MerchantID: mid.UUID(), CustomerID: bad.UUID(), CollectionMethod: CollectionChargeAutomatically, Tax: []byte(`{}`), BillingContacts: []byte(`[{"email":1}]`), Now: clock.Now()})
		}))
		n, err := service.FinalizeDueUsage(ctx, clock.Now())
		require.Error(t, err)
		require.GreaterOrEqual(t, n, 1)
		orders, _ := count(good)
		require.Equal(t, 1, orders)
		orders, _ = count(bad)
		require.Zero(t, orders)
		require.True(t, settings(bad).UsageCollectionDue, "failed account remains due for retry")
	})
	t.Run("active automatic policy offers no undeclared currency credit", func(t *testing.T) {
		id := customer()
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			if _, err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "EUR", BillingMode: BillingModeArrears, Now: clock.Now()}); err != nil {
				return err
			}
			return q.SetMoneyAccountCreditLimit(ctx, gen.SetMoneyAccountCreditLimitParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "EUR", CreditLimit: 100_000_000, Now: clock.Now()})
		}))
		capacity, err := service.GetAdmissionCapacity(ctx, id, "EUR")
		require.NoError(t, err)
		require.Zero(t, capacity.CreditLimit)
		require.Equal(t, BillingModePrepaid, capacity.BillingMode)
		manual := service.WithDatabase(database)
		manual.UsageBillingEnabled = false
		capacity, err = manual.GetAdmissionCapacity(ctx, id, "EUR")
		require.NoError(t, err)
		require.EqualValues(t, 100_000_000, capacity.CreditLimit, "manual enterprise credit remains available outside automatic policy")
	})
	t.Run("merchant cap changes cannot strand customer minimums", func(t *testing.T) {
		id := customer()
		accrue(id, 1)
		minimum := int64(50_000_000)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return gen.New(tx).UpdateUsageBillingAccount(ctx, gen.UpdateUsageBillingAccountParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "USD", SetMinimum: true, MinimumInvoiceAmount: &minimum, Now: clock.Now()})
		}))
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			bad, err := database.Gen(ctx).FindInvalidUsageMinimum(ctx, gen.FindInvalidUsageMinimumParams{MerchantID: mid.UUID(), Currency: "USD", DefaultCredit: 20_000_000, DefaultMinimum: 5_000_000})
			require.NoError(t, err)
			require.Equal(t, id.UUID(), bad)
			return nil
		}))
		cfg.settings.UsageBilling.Currencies[0].CreditLimit = 20_000_000
		_, err := service.GetAdmissionCapacity(ctx, id, "USD")
		require.ErrorContains(t, err, "minimum invoice exceeds")
		cfg.settings.UsageBilling.Currencies[0].CreditLimit = 100_000_000
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return gen.New(tx).SetMoneyAccountCreditLimit(ctx, gen.SetMoneyAccountCreditLimitParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "USD", CreditLimit: 0, Now: clock.Now()})
		}))
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := database.Gen(ctx).FindInvalidUsageMinimum(ctx, gen.FindInvalidUsageMinimumParams{MerchantID: mid.UUID(), Currency: "USD", DefaultCredit: 20_000_000, DefaultMinimum: 5_000_000})
			require.ErrorIs(t, err, pgx.ErrNoRows)
			return nil
		}))
	})
	t.Run("policy edits serialize without consuming the one connection pool", func(t *testing.T) {
		config := pool.Config().Copy()
		config.MaxConns = 1
		single, err := pgxpool.NewWithConfig(ctx, config)
		require.NoError(t, err)
		defer single.Close()
		one, err := db.NewWithPGXPool(single, schema)
		require.NoError(t, err)
		held, err := single.Acquire(ctx)
		require.NoError(t, err)
		unlock, err := one.LockUsagePolicy(ctx, mid)
		require.NoError(t, err)
		waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_, err = one.LockUsagePolicy(waiting, mid)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		unlock()
		held.Release()
		unlock, err = one.LockUsagePolicy(ctx, mid)
		require.NoError(t, err)
		unlock()
	})
	t.Run("configuration inheritance is explicit", func(t *testing.T) {
		id := customer()
		accrue(id, 1)
		row := settings(id)
		require.False(t, row.CreditLimitOverride)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return gen.New(tx).SetMoneyAccountCreditLimit(ctx, gen.SetMoneyAccountCreditLimitParams{MerchantID: mid.UUID(), CustomerID: id.UUID(), Currency: "USD", CreditLimit: 0, Now: clock.Now()})
		}))
		row = settings(id)
		require.True(t, row.CreditLimitOverride)
		require.Zero(t, usageCreditLimit(row, &cfg.settings.UsageBilling.Currencies[0]))
	})
}
