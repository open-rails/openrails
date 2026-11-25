//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// usageCatalog declares everything that needs the event store.
const usageCatalog = `schema_version: 1
meters:
  tokens: {event_type: chat, value_property: tokens, aggregation: sum}
limits:
  daily:
    unit: tokens
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    counts: [{meter: tokens}]
products:
  basic:
    display_name: Basic
    limits: {daily: "100"}
    rate_cards: [{meter: tokens, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
`

// requireNoStore is event_store_required, a definite 409, never unreachable.
func requireNoStore(t *testing.T, err error) *billing.StatusError {
	t.Helper()
	failure := requireCode(t, err, billing.CodeEventStoreRequired)
	require.Equal(t, http.StatusConflict, failure.Status)
	require.ErrorIs(t, err, billing.ErrEventStoreRequired)
	require.NotErrorIs(t, err, billing.ErrUnreachable)
	return failure
}

// usageFlag is GET /v1/config's usage.
func usageFlag(t *testing.T, a *authorityWorld) bool {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/config", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var public billing.PublicConfig
	require.NoError(t, json.Unmarshal(raw, &public))
	return public.Usage
}

func TestEventStore(t *testing.T) {
	t.Run("OR-J24-S39/without-clickhouse-usage-answers-event-store-required-and-the-rest-works", func(t *testing.T) {
		t.Run("usage-catalog-routes-client-and-gate", noStoreRefusals)
		t.Run("usage-settings", noStoreSettings)
		t.Run("subscriptions-credit-packs-and-running-charges", noStorePurchases)
		t.Run("usage-orders", noStoreUsageOrders)
	})
	t.Run("OR-J24-S40/new-migrates-clickhouse-idempotently-and-refuses-another-store", storeMigrationsAndIdentity)
	t.Run("OR-J24-S66/two-engines-migrating-one-fresh-clickhouse-database-together-both-start", concurrentStoreMigrations)
	t.Run("OR-J24-S41/public-config-reports-whether-usage-is-served", func(t *testing.T) {
		require.True(t, usageFlag(t, newAuthorityWorld(t)))
		require.False(t, usageFlag(t, newAuthorityWorld(t, withoutEventStore)))
	})
}

func noStoreRefusals(t *testing.T) {
	a := newAuthorityWorld(t, withoutEventStore)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	document, err := catalog.ParseApplicationYAML([]byte(usageCatalog))
	require.NoError(t, err)
	for _, client := range []*openrails.Client{a.alpha, a.alphaRemote} {
		_, err := client.ApplyCatalog(ctx, document, billing.ApplyCatalogParams{})
		failure := requireNoStore(t, err)
		require.NotNil(t, failure.Param)
		require.Equal(t, "meters.tokens", *failure.Param)
		require.Equal(t, []any{"meters.tokens", "limits.daily", "products.basic.limits", "products.basic.rate_cards"}, failure.Metadata["paths"])
	}
	meters, err := a.alpha.ListMeters(ctx, billing.PageRequest{})
	require.NoError(t, err)
	require.Empty(t, meters.Items, "a refused document applies nothing")
	plain, err := catalog.ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  basic:\n    display_name: Basic\n    prices:\n      monthly: {amount: 5 USD, billing_interval: 1 month}\n"))
	require.NoError(t, err)
	_, err = a.alpha.ApplyCatalog(ctx, plain, billing.ApplyCatalogParams{})
	require.NoError(t, err, "products and prices need no event store")

	// Every event-store route answers 409 once its caller is authenticated,
	// in process and over HTTP alike.
	_, err = a.alpha.SetMeter(ctx, "tokens", billing.SetMeterParams{EventType: "chat", ValueProperty: "tokens", Aggregation: catalog.AggregationSum})
	requireNoStore(t, err)
	_, err = a.alphaRemote.SetLimit(ctx, "daily", billing.SetLimitParams{Unit: "tokens"})
	requireNoStore(t, err)
	_, err = a.alpha.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: billing.LeaseID(uuid.Must(uuid.NewV7()))})
	requireNoStore(t, err)
	_, err = a.alphaRemote.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: billing.LeaseID(uuid.Must(uuid.NewV7()))})
	requireNoStore(t, err)
	_, err = a.alpha.ListCustomerLimits(ctx, billing.CustomerLimitListParams{CustomerID: customer})
	requireNoStore(t, err)
	_, err = a.alphaRemote.AdjustCustomerLimit(ctx, customer, billing.AdjustCustomerLimitParams{Limit: "daily", Amount: 1, SourceID: "no-store"})
	requireNoStore(t, err)
	for _, path := range []string{"/v1/me/usage?currency=USD", "/v1/me/limits"} {
		status, raw := a.request(t, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusUnauthorized, status, "authentication comes first: %s", raw)
		status, raw = a.request(t, http.MethodGet, path, a.aliceToken, nil)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"event_store_required"`)
	}

	// A metrics query naming usage is refused; one that does not answers.
	week := &billing.MetricsRange{From: a.clock.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339), To: a.clock.Now().Add(time.Second).Format(time.RFC3339)}
	_, err = a.alpha.QueryMetrics(ctx, billing.QueryMetricsParams{Measures: []string{"usage_charged"}, Range: week})
	requireNoStore(t, err)
	_, err = a.alpha.QueryMetrics(ctx, billing.QueryMetricsParams{Measures: []string{"rebills"}, Range: week})
	require.NoError(t, err)

	// An embedded engine's gate refuses at construction; a remote gate's first
	// sync refuses every later request at once.
	_, err = openrails.NewGate(a.alpha, openrails.GateDeps{})
	require.ErrorIs(t, err, billing.ErrEventStoreRequired)
	remote, _ := gateRemote(t, a)
	g := newGate(t, remote, openrails.GateDeps{})
	work := billing.AdmitParams{RequestID: "gate-no-store", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"tokens": 1}, Amount: ptr(int64(0))}}
	admitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = g.Admit(admitCtx, work)
	require.ErrorIs(t, err, billing.ErrEventStoreRequired)
	require.NotErrorIs(t, err, openrails.ErrUnavailable)
	started := time.Now()
	work.RequestID = "gate-no-store-again"
	_, err = g.Admit(admitCtx, work)
	require.ErrorIs(t, err, billing.ErrEventStoreRequired)
	require.Less(t, time.Since(started), time.Second, "the refusal is kept, not asked again")
	require.False(t, usageFlag(t, a))
}

func noStoreSettings(t *testing.T) {
	a, _ := newPSPWorld(t, withoutEventStore)
	ctx := t.Context()
	window := []billing.BudgetWindow{{Key: "day", WindowSeconds: 86400, Limit: 1_000_000, Currency: "USD"}}
	for _, settings := range []billing.MerchantSettings{
		{DelegatedInvokerWastedSpendLimits: window},
		{BillingPolicies: []billing.BillingPolicy{{Name: "capped", Kind: "window_spend_cap", SpendWindows: window}}},
		{BillingPolicies: []billing.BillingPolicy{{Name: "grace", Kind: "outstanding_cap", BadSpendWindows: window}}},
		{BillingPolicies: []billing.BillingPolicy{{Name: "rate", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000}}},
	} {
		_, err := a.alphaRemote.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{MerchantSettings: &settings})
		requireNoStore(t, err)
	}
	updated, err := a.alphaRemote.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha without usage")})
	require.NoError(t, err)
	require.Equal(t, "Alpha without usage", updated.DisplayName)
}

func noStorePurchases(t *testing.T) {
	a, provider, sub := purchasedMembershipWith(t, 24, withoutEventStore)
	require.Equal(t, "active", string(sub.Status))
	ctx := t.Context()
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	pack, err := a.alpha.CreateProduct(ctx, billing.CreateProductParams{Key: "credit-pack", DisplayName: "Credit pack", Ownership: catalog.OwnershipConsumable,
		CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(int64(25_000_000)), ExpiresAfterDays: ptr(365)}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(ctx, billing.CreatePriceParams{ProductID: pack.ID, Key: "pack", UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
		Payment: &billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "credit-pack"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	b, err := a.alpha.GetBalance(ctx, customer, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	require.EqualValues(t, 25_000_000, b.AvailableAmount)

	charge := billing.StartRunningChargeParams{Reference: "no-store", CustomerID: customer, Currency: "USD", RatePerHour: 3600}
	_, err = a.alpha.StartRunningCharge(ctx, charge)
	require.NoError(t, err)
	a.clock.Advance(900 * time.Second)
	stopped, err := a.alpha.StopRunningCharge(ctx, charge.Reference, billing.StopRunningChargeParams{StoppedAt: ptr(a.clock.Now())})
	require.NoError(t, err)
	require.EqualValues(t, 900, stopped.ChargedAmount)
	require.EqualValues(t, 900, stopped.PostedAmount)
}

// A running charge leaves owed what the balance does not cover, and a usage
// order bills it: neither needs the event store.
func noStoreUsageOrders(t *testing.T) {
	a, p, customer := usageBillingAccount(t, false, withoutEventStore)
	ctx := t.Context()
	charge := billing.StartRunningChargeParams{Reference: "owed", CustomerID: customer, Currency: "USD", RatePerHour: 10_000_000}
	_, err := a.alpha.StartRunningCharge(ctx, charge)
	require.NoError(t, err)
	a.clock.Advance(2 * time.Hour)
	stopped, err := a.alpha.StopRunningCharge(ctx, charge.Reference, billing.StopRunningChargeParams{StoppedAt: ptr(a.clock.Now())})
	require.NoError(t, err)
	require.EqualValues(t, 20_000_000, stopped.PostedAmount)
	fleet := startFleet(t, a.world)
	a.clock.Advance(12 * time.Hour)
	runUsageDue(t, fleet)
	require.Eventually(t, func() bool {
		rows, err := a.alpha.ListOrders(ctx, billing.OrderListParams{CustomerID: customer})
		return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
	}, 15*time.Second, 20*time.Millisecond)
	order := oneUsageOrder(t, a, customer)
	require.EqualValues(t, 20_000_000, order.Total)
	require.EqualValues(t, 20_000_000, order.Usage.AmountPaid)
	p.money(t, 1, 2000)
}

func storeMigrationsAndIdentity(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	conn := w.deps.ClickHouse
	reopen := func(cfg openrails.Config, deps openrails.Deps) error {
		t.Helper()
		require.NoError(t, w.engine.Close(context.Background()))
		w.engine = nil
		engine, err := openrails.New(ctx, cfg, deps)
		if err == nil {
			w.engine = engine
		}
		return err
	}
	tables := func() []string {
		t.Helper()
		rows, err := conn.Query(ctx, "SELECT name FROM system.tables WHERE database = ? ORDER BY name", w.schema)
		require.NoError(t, err)
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		return names
	}
	installed := []string{"limit_usage", "limit_usage_mv", "schema_migrations", "store", "usage_events", "usage_events_mv", "usage_history", "usage_history_charges_mv",
		"usage_history_events_mv", "usage_intake", "usage_minutes", "usage_minutes_mv", "usage_quantities", "usage_quantities_mv"}
	require.Equal(t, installed, tables())

	// A restart and an explicit migration change nothing.
	require.NoError(t, reopen(w.cfg, w.deps))
	require.NoError(t, openrails.MigrateClickHouse(ctx, w.pool, conn, w.cfg.ClickHouse))
	require.Equal(t, installed, tables())
	var applied uint64
	require.NoError(t, conn.QueryRow(ctx, "SELECT count() FROM `"+w.schema+"`.schema_migrations FINAL").Scan(&applied))
	require.EqualValues(t, 4, applied)

	// An emptied ClickHouse is not the store this database's usage is in:
	// once migrated it is another store.
	require.NoError(t, conn.Exec(ctx, "DROP DATABASE `"+w.schema+"` SYNC"))
	err := openrailsNewErr(ctx, w.cfg, w.deps)
	require.ErrorContains(t, err, "empty or different ClickHouse")

	// Without ClickHouse the database starts: usage is off.
	without := w.deps
	without.ClickHouse = nil
	engine, err := openrails.New(ctx, w.cfg, without)
	require.NoError(t, err)
	w.engine = engine
	client, err := declaredMerchant(engine, "alpha")
	require.NoError(t, err)
	_, err = client.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: billing.LeaseID(uuid.Must(uuid.NewV7()))})
	requireNoStore(t, err)
}

// openrailsNewErr is New's refusal; an engine it opens is closed.
func openrailsNewErr(ctx context.Context, cfg openrails.Config, deps openrails.Deps) error {
	engine, err := openrails.New(ctx, cfg, deps)
	if err == nil {
		_ = engine.Close(context.Background())
	}
	return err
}

// concurrentStoreMigrations starts two engines together on a fresh schema and
// a fresh ClickHouse database, as replicas of a new deployment do: they take
// turns migrating, so each migration is recorded once and both serve usage.
func concurrentStoreMigrations(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	fresh := "critical_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg := w.cfg
	cfg.Database.Schema, cfg.Database.RiverSchema, cfg.ClickHouse.Database = fresh, fresh, fresh
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.deps.ClickHouse.Exec(cleanup, "DROP DATABASE IF EXISTS `"+fresh+"` SYNC")
		_, _ = w.pool.Exec(cleanup, "DROP SCHEMA IF EXISTS "+pgx.Identifier{fresh}.Sanitize()+" CASCADE")
	})
	var engines [2]*openrails.Engine
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range engines {
		wg.Go(func() {
			<-start
			engines[i], errs[i] = openrails.New(ctx, cfg, w.deps)
		})
	}
	close(start)
	wg.Wait()
	for i, engine := range engines {
		require.NoError(t, errs[i], "engine %d", i)
		t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
	}
	var recorded, stores uint64
	require.NoError(t, w.deps.ClickHouse.QueryRow(ctx, "SELECT count() FROM `"+fresh+"`.schema_migrations").Scan(&recorded))
	require.EqualValues(t, 4, recorded, "each migration ran and was recorded once")
	require.NoError(t, w.deps.ClickHouse.QueryRow(ctx, "SELECT count() FROM `"+fresh+"`.store").Scan(&stores))
	require.EqualValues(t, 1, stores, "one store identity")
	first, err := declaredMerchant(engines[0], "alpha")
	require.NoError(t, err)
	usageMeter(t, first, 100)
	for i, engine := range engines {
		client, err := declaredMerchant(engine, "alpha")
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.New())
		fund(t, client, customer, "funded", 1000)
		settleUsage(t, client, newLease(), numbered(1, usageInput(customer, fmt.Sprintf("after-the-race-%d", i), 1))...)
		balance(t, client, customer, 900, 100, 1000)
	}
}
