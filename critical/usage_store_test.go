//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// Usage lives in the event store; these read it there only to show what a
// scenario left, never as a business API.

// storeCount is a count the world's event store answers; {db} names its
// database and {m} the alpha merchant.
func storeCount(t *testing.T, w *world, query string) uint64 {
	t.Helper()
	query = strings.NewReplacer("{db}", "`"+w.schema+"`", "{m}", "toUUID('"+w.alpha.MerchantID().String()+"')").Replace(query)
	var n uint64
	require.NoError(t, w.deps.ClickHouse.QueryRow(t.Context(), query).Scan(&n))
	return n
}

// pricedMeter is the compute meter priced by price.
func pricedMeter(t *testing.T, client *openrails.Client, price catalog.RatePrice) billing.SetMeterParams {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage-compute", DisplayName: "Usage compute"})
	require.NoError(t, err)
	price.Currency = "USD"
	params := billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: price})}
	_, err = client.SetMeter(t.Context(), "compute", params)
	require.NoError(t, err)
	return params
}

func tiers(mode catalog.TierMode, first, rest, upTo int64) catalog.RatePrice {
	return catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: mode,
		Tiers: []catalog.RateTier{{UpTo: &upTo, UnitAmount: first}, {UnitAmount: rest}}}}
}

// owed is the customer's owed USD.
func owed(t *testing.T, client *openrails.Client, customer billing.CustomerID) int64 {
	t.Helper()
	b, err := client.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	return b.OwedAmount
}

// downConn is the world's ClickHouse, out of reach while down is set.
type downConn struct {
	driver.Conn
	down atomic.Bool
}

var errStoreDown = errors.New("critical: ClickHouse is out of reach")

func (c *downConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	if c.down.Load() {
		return nil, errStoreDown
	}
	return c.Conn.Query(ctx, query, args...)
}

func (c *downConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	if c.down.Load() {
		return downRow{}
	}
	return c.Conn.QueryRow(ctx, query, args...)
}

func (c *downConn) Exec(ctx context.Context, query string, args ...any) error {
	if c.down.Load() {
		return errStoreDown
	}
	return c.Conn.Exec(ctx, query, args...)
}

func (c *downConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	if c.down.Load() {
		return nil, errStoreDown
	}
	return c.Conn.PrepareBatch(ctx, query, opts...)
}

type downRow struct{}

func (downRow) Err() error           { return errStoreDown }
func (downRow) Scan(...any) error    { return errStoreDown }
func (downRow) ScanStruct(any) error { return errStoreDown }

func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	failure := requireCode(t, err, billing.CodeServiceUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, failure.Status)
}

func TestUsageStore(t *testing.T) {
	t.Run("OR-J24-S47/a-resend-after-a-lost-commit-inserts-nothing-twice", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		items := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 1), usageInput(customer, "c", 1))
		settleUsage(t, w.alpha, lease, items...)
		balance(t, w.alpha, customer, 700, 300, 1000)
		// Fixture: the sync's events reached the store, its transaction never
		// committed.
		leases, unposted := pgx.Identifier{w.schema, "leases"}.Sanitize(), pgx.Identifier{w.schema, "usage_unposted"}.Sanitize()
		_, err := w.pool.Exec(t.Context(), "UPDATE "+leases+" SET settled_through_n = 0, receipts = '[]'::jsonb WHERE merchant_id = $1 AND id = $2",
			w.alpha.MerchantID().UUID(), lease.UUID())
		require.NoError(t, err)
		_, err = w.pool.Exec(t.Context(), "DELETE FROM "+unposted+" WHERE merchant_id = $1", w.alpha.MerchantID().UUID())
		require.NoError(t, err)
		balance(t, w.alpha, customer, 1000, 0, 1000)

		// The resend finds them and redoes only PostgreSQL's side.
		settleUsage(t, w.alpha, lease, items...)
		balance(t, w.alpha, customer, 700, 300, 1000)
		require.EqualValues(t, 3, storeCount(t, w, "SELECT count() FROM {db}.usage_events WHERE merchant_id = {m}"), "raw holds each item once")
		require.EqualValues(t, 3, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"))
		require.EqualValues(t, 3, storeCount(t, w, "SELECT toUInt64(sum(quantity)) FROM {db}.usage_quantities WHERE merchant_id = {m}"), "the rollups agree")
		require.EqualValues(t, 3, storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"))
	})

	t.Run("OR-J24-S48/while-clickhouse-is-out-of-reach-usage-settles-nothing-and-other-money-moves", func(t *testing.T) {
		var conn *downConn
		w := newWorld(t, func(w *world) {
			conn = &downConn{Conn: w.deps.ClickHouse}
			w.deps.ClickHouse = conn
		})
		usageMeter(t, w.alpha, 100)
		user, _ := fundedCustomer(t, w)
		other := billing.CustomerID(uuid.New())
		fund(t, w.alpha, other, "other", 1000)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(user, "before", 1))...)

		conn.down.Store(true)
		_, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, usageInput(user, "during", 2))})
		requireUnavailable(t, err)
		_, err = w.alpha.GetBalance(t.Context(), user, billing.GetBalanceParams{Currency: "USD"})
		requireUnavailable(t, err)
		_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: user, Currency: "USD", Amount: 500, Source: "critical", SourceID: "top-up"}})
		requireUnavailable(t, err)
		// A customer without usage is unaffected.
		fund(t, w.alpha, other, "other-top-up", 500)
		balance(t, w.alpha, other, 1500, 0, 1500)

		conn.down.Store(false)
		fund(t, w.alpha, user, "top-up", 500)
		balance(t, w.alpha, user, 1400, 0, 1400)
		out := settleUsage(t, w.alpha, lease, numbered(2, usageInput(user, "during", 2))...)
		require.EqualValues(t, 2, out.Usage.ThroughN)
		balance(t, w.alpha, user, 1200, 200, 1400)
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"), "every item settled once")
	})

	t.Run("OR-J24-S49/a-period-posts-the-price-of-its-total", func(t *testing.T) {
		t.Run("graduated-across-two-postings", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			pricedMeter(t, w.alpha, tiers(catalog.TierModeGraduated, 10, 5, 10))
			customer, _ := fundedCustomer(t, w)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "first", 6))...)
			balance(t, w.alpha, customer, 940, 60, 1000)
			fund(t, w.alpha, customer, "top-up", 100)
			balance(t, w.alpha, customer, 1040, 0, 1040)
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "second", 6))...)
			balance(t, w.alpha, customer, 990, 50, 1040)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			balance(t, w.alpha, customer, 990, 0, 990)
			deductions := usageDeductions(t, w.alpha, customer)
			require.Len(t, deductions, 2)
			require.EqualValues(t, -50, deductions[0].Amount, "12 units cost 110; the first posting took 60")
			require.EqualValues(t, -60, deductions[1].Amount)
		})
		t.Run("a-volume-crossing-is-carried-and-credited-at-period-close", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			pricedMeter(t, w.alpha, tiers(catalog.TierModeVolume, 10, 5, 10))
			customer, _ := fundedCustomer(t, w)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "first", 8))...)
			fund(t, w.alpha, customer, "top-up", 100)
			balance(t, w.alpha, customer, 1020, 0, 1020)
			// 12 units cost 60 at the cheaper band: 20 less than posted.
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "second", 4))...)
			balance(t, w.alpha, customer, 1020, 0, 1020)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			require.Len(t, usageDeductions(t, w.alpha, customer), 1, "the remainder is carried while late usage may come")
			w.clock.Advance(70 * 24 * time.Hour)
			closeMonths(t, fleet)
			balance(t, w.alpha, customer, 1040, 0, 1040)
			deductions := usageDeductions(t, w.alpha, customer)
			require.Len(t, deductions, 2)
			require.EqualValues(t, 20, deductions[0].Amount, "the period's close credits it back")
			require.EqualValues(t, -20, deductions[0].Usage.Amount)
		})
		t.Run("a-card-revision-mid-period-is-priced-card-by-card", func(t *testing.T) {
			w := newWorld(t, earlyInMonth)
			_, params := usageMeter(t, w.alpha, 100)
			customer, _ := fundedCustomer(t, w)
			lease := newLease()
			settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "old-card", 2))...)
			params.RateCard.Value.Price.PerUnit.UnitAmount = 300
			_, err := w.alpha.SetMeter(t.Context(), "compute", params)
			require.NoError(t, err)
			settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "new-card", 1))...)
			fleet := startFleet(t, w)
			nextMonth(w)
			closeMonths(t, fleet)
			deductions := usageDeductions(t, w.alpha, customer)
			require.Len(t, deductions, 1)
			require.EqualValues(t, -500, deductions[0].Amount)
			require.Equal(t, []billing.UsageDeductionLine{{MeterKey: "compute", Requests: 2, Quantity: 3, Amount: 500}}, deductions[0].Usage.Lines)
		})
	})

	t.Run("OR-J24-S56/a-month-closes-at-its-end-in-the-merchants-accounting-zone", func(t *testing.T) {
		ny, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
			setCfgMerchant(&w.cfg, "alpha", m)
			// 02:00 UTC on the 1st is still the month's last evening in New York.
			now := w.clock.Now().UTC()
			w.clock.Advance(time.Date(now.Year(), now.Month()+1, 1, 2, 0, 0, 0, time.UTC).Sub(now))
		})
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "last-evening", 2))...)
		fleet := startFleet(t, w)
		closeMonths(t, fleet)
		require.Empty(t, usageDeductions(t, w.alpha, customer), "New York's month has not ended")
		local := w.clock.Now().In(ny)
		end := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, ny)
		w.clock.Advance(end.Add(time.Minute).Sub(w.clock.Now()))
		closeMonths(t, fleet)
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 1)
		require.EqualValues(t, -200, deductions[0].Amount)
		require.True(t, end.Equal(deductions[0].Usage.Through), "cut at New York's month end, not UTC's: %s", deductions[0].Usage.Through)
	})

	t.Run("OR-J24-S57/usage-history-folds-into-the-merchants-days-and-regroups-when-its-zone-changes", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		zone := func(name string) {
			t.Helper()
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
				MerchantSettings: &billing.MerchantSettings{Accounting: &billing.AccountingSettings{Timezone: name}}})
			require.NoError(t, err)
		}
		zone("Asia/Kolkata")
		kolkata, err := time.LoadLocation("Asia/Kolkata")
		require.NoError(t, err)
		pricedMeter(t, a.alphaRemote, tiers(catalog.TierModeGraduated, 100, 50, 3))
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		// 23:30 on the 10th in Kolkata is 18:00 UTC; 00:30 on the 11th is
		// 19:00 UTC, the same UTC day.
		now := a.clock.Now().In(kolkata)
		evening := time.Date(now.Year(), now.Month()+1, 10, 23, 30, 0, 0, kolkata)
		a.clock.Advance(evening.Sub(a.clock.Now()))
		lease := newLease()
		settleUsage(t, a.alphaRemote, lease, numbered(1, usageInput(alice, "late-evening", 2))...)
		a.clock.Advance(time.Hour)
		settleUsage(t, a.alphaRemote, lease, numbered(2, usageInput(alice, "after-midnight", 4))...)
		report := func(day time.Time) billing.UsageRow {
			t.Helper()
			q := url.Values{"currency": {"USD"}, "from": {day.Format(time.RFC3339)}, "to": {day.Add(time.Minute).Format(time.RFC3339)}}
			status, raw := a.request(t, http.MethodGet, "/v1/me/usage?"+q.Encode(), a.aliceToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var page billing.Usage
			require.NoError(t, json.Unmarshal(raw, &page))
			require.Len(t, page.Rows, 1)
			return page.Rows[0]
		}
		// 2 units at 100; then 1 more at 100 and 3 at 50.
		first, second := report(evening), report(evening.Add(time.Hour))
		require.Equal(t, [2]int64{1, 200}, [2]int64{first.EventCount, first.Amount}, "23:30 is the 10th in Kolkata")
		require.Equal(t, [2]int64{1, 250}, [2]int64{second.EventCount, second.Amount}, "00:30 is the 11th")

		// The same history read in UTC is one day.
		zone("UTC")
		utc := report(evening)
		require.Equal(t, [2]int64{2, 450}, [2]int64{utc.EventCount, utc.Amount}, "a zone change regroups the past")

		fleet := startFleet(t, a.world)
		nextMonth(a.world)
		closeMonths(t, fleet)
		require.EqualValues(t, first.Amount+second.Amount, owed(t, a.alphaRemote, alice), "the days sum to what posts")
	})

	t.Run("OR-J24-S50/late-usage-after-its-month-posted-posts-the-difference", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fleet := startFleet(t, w)
		lease := newLease()
		occurred := w.clock.Now().UTC()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "in-month", 2))...)
		nextMonth(w)
		closeMonths(t, fleet)
		require.EqualValues(t, 200, owed(t, w.alpha, customer))
		w.clock.Advance(19 * 24 * time.Hour)
		late := usageInput(customer, "late", 1)
		late.OccurredAt = occurred
		settleUsage(t, w.alpha, lease, numbered(2, late)...)
		closeMonths(t, fleet)
		require.EqualValues(t, 300, owed(t, w.alpha, customer), "the late item posts at once: its month ended")
		require.Len(t, usageDeductions(t, w.alpha, customer), 2)
	})

	t.Run("OR-J24-S51/every-limit-kind-reads-its-use-from-the-event-store", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  fixed: {unit: units, reset: {kind: fixed, window_seconds: 3600, anchor: first_use}, counts: [{meter: work}]}
  session: {unit: units, reset: {kind: session, window_seconds: 3600}, counts: [{meter: work}]}
  rolling: {unit: units, reset: {kind: rolling, window_seconds: 600}, counts: [{meter: work}]}
  rate: {unit: units, reset: {kind: rate, window_seconds: 600}, counts: [{meter: work}]}
  period: {unit: units, reset: {kind: billing_period}, counts: [{meter: work}]}
  ever: {unit: units, reset: {kind: lifetime}, counts: [{meter: work}]}
products:
  plan: {display_name: Plan, limits: {fixed: "100", session: "100", rolling: "100", rate: "600", period: "100", ever: "100"}}
`)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24*90)
		lease := newLease()
		admitted := w.clock.Now().UTC()
		settleUsage(t, w.alpha, lease, numbered(1, workReport(customer, "first", 10))...)
		for _, key := range []string{"fixed", "session", "rolling", "rate", "period", "ever"} {
			require.EqualValues(t, 10, customerLimit(t, w.alpha, customer, key).Used, key)
		}
		// The rate limit drains at its capacity per window: 600 in 600 s.
		w.clock.Advance(2 * time.Minute)
		require.Zero(t, customerLimit(t, w.alpha, customer, "rate").Used)
		require.EqualValues(t, 10, customerLimit(t, w.alpha, customer, "rolling").Used)
		// Past the fixed window's end, an item admitted before it counts in the
		// window it was admitted in.
		w.clock.Advance(time.Hour)
		before := workReport(customer, "admitted-before-the-reset", 5)
		before.AdmittedAt = &admitted
		settleUsage(t, w.alpha, lease, numbered(2, before)...)
		require.Zero(t, customerLimit(t, w.alpha, customer, "fixed").Used, "the new window")
		require.EqualValues(t, 15, customerLimit(t, w.alpha, customer, "ever").Used)
		require.Zero(t, customerLimit(t, w.alpha, customer, "rolling").Used, "the rolling window passed")
	})

	t.Run("OR-J24-S52/new-refuses-a-database-with-unposted-usage-without-its-store", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "unposted", 2))...)
		ctx := t.Context()
		require.NoError(t, w.engine.Close(context.Background()))
		without := w.deps
		without.ClickHouse = nil
		err := openrailsNewErr(ctx, w.cfg, without)
		require.ErrorIs(t, err, billing.ErrEventStoreRequired)
		require.ErrorContains(t, err, w.alpha.MerchantID().String())

		// A lost store: an empty one is refused, and attaching it discards the
		// unposted usage only when told to, on the record.
		emptied := w.cfg
		emptied.ClickHouse.Database = w.schema + "_replacement"
		require.ErrorContains(t, openrailsNewErr(ctx, emptied, w.deps), "empty or different ClickHouse")
		attach := map[string]any{"clickhouse": map[string]string{"addr": os.Getenv("OPENRAILS_CRITICAL_CLICKHOUSE_ADDR"), "database": emptied.ClickHouse.Database}}
		out, err := hostCLICommand(t, w, attach, "usage", "attach", "--actor", "critical", "--reason", "drill").CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(out), "--discard-unposted")
		out, err = hostCLICommand(t, w, attach, "usage", "attach", "--actor", "critical", "--reason", "drill", "--discard-unposted").CombinedOutput()
		require.NoError(t, err, string(out))
		require.Contains(t, string(out), w.alpha.MerchantID().String())
		t.Cleanup(func() {
			_ = w.deps.ClickHouse.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+emptied.ClickHouse.Database+"` SYNC")
		})
		engine, err := openrails.New(ctx, emptied, w.deps)
		require.NoError(t, err)
		w.engine = engine
		client, err := declaredMerchant(engine, "alpha")
		require.NoError(t, err)
		balance(t, client, customer, 1000, 0, 1000)
	})

	t.Run("OR-J24-S53/erasure-clears-invoker-and-metadata-and-keeps-quantities-and-money", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) { w.deps.UserInfo = nil })
		usageMeter(t, a.alpha, 100)
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "erasure"})
		require.NoError(t, err)
		id := uuid.NewString()
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "erased", "displayName": "Erased", "active": true}
		status, raw := a.request(t, "POST", "/v1/app/scim/v2/Users", provisioner.Token, user)
		require.Equal(t, http.StatusCreated, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(id))
		fund(t, a.alpha, customer, "erased-funds", 1000)
		settleUsage(t, a.alpha, newLease(), numbered(1, usageInput(customer, "personal", 2))...)
		balance(t, a.alpha, customer, 800, 200, 1000)
		w := a.world
		require.EqualValues(t, 1, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m} AND invoker != '' AND metadata != ''"))

		status, raw = a.request(t, "DELETE", "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Zero(t, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m} AND (invoker != '' OR metadata != '')"))
		require.Zero(t, storeCount(t, w, "SELECT count() FROM {db}.usage_history WHERE merchant_id = {m} AND invoker != ''"))
		require.EqualValues(t, 2, storeCount(t, w, "SELECT toUInt64(sum(quantity)) FROM {db}.usage_history WHERE merchant_id = {m}"), "quantities stay")
		balance(t, a.alpha, customer, 800, 200, 1000)
	})

	t.Run("OR-J24-S54/an-archive-carries-the-merchants-usage-and-restores-it-into-an-empty-store", func(t *testing.T) {
		source := newWorld(t)
		usageMeter(t, source.alpha, 100)
		customer, _ := fundedCustomer(t, source)
		lease := newLease()
		settleUsage(t, source.alpha, lease, numbered(1, usageInput(customer, "archived", 3))...)
		syncLease(t, source.alpha, billing.SyncLeasesParams{LeaseID: lease, Close: true})
		balance(t, source.alpha, customer, 700, 300, 1000)
		archive := archiveBytes(t, source.alpha)

		_, refused := archiveTarget(t, source, withoutEventStore)
		_, err := refused.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.Error(t, err, "usage needs an event store")

		occupied, client := archiveTarget(t, source)
		require.NoError(t, occupied.deps.ClickHouse.Exec(t.Context(), "INSERT INTO `"+occupied.schema+"`.usage_events (merchant_id, event_id, customer_id, currency, event_type, outcome) VALUES (toUUID(?), generateUUIDv4(), toUUID(?), 'USD', 'compute', 'succeeded')",
			source.alpha.MerchantID().String(), customer.String()))
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.Error(t, err, "a merchant with usage in the store is not empty")

		target, client := archiveTarget(t, source)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		balance(t, client, customer, 700, 300, 1000)
		require.EqualValues(t, 1, storeCount(t, target, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = toUUID('"+source.alpha.MerchantID().String()+"')"))
	})

	t.Run("OR-J24-S55/equal-lease-ids-and-numbers-under-two-merchants-stay-apart", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		usageMeter(t, w.beta, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "alpha", 1000)
		_, err := w.beta.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "beta"}})
		require.NoError(t, err)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "same", 2))...)
		settleUsage(t, w.beta, lease, numbered(1, usageInput(customer, "same", 3))...)
		balance(t, w.alpha, customer, 800, 200, 1000)
		balance(t, w.beta, customer, 700, 300, 1000)
		require.EqualValues(t, 1, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"))
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL"), "each merchant's item is its own event")
	})
}
