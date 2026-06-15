//go:build integration

package analytics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	chmod "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestAdminMetricsCrossTenantIsolation proves the issue #232 fix end to end:
// with daily_metrics rows seeded for two distinct tenants, every
// AdminMetricsService read scoped to tenant A returns ONLY tenant A's data and
// never tenant B's, while the explicit platform-superadmin cross-tenant path
// sees both. This is the regression guard for the original leak where a tenant
// operator could read platform-wide metrics.
func TestAdminMetricsCrossTenantIsolation(t *testing.T) {
	ctx := context.Background()

	const (
		dbName = "test_analytics"
		dbUser = "test_user"
		dbPass = "test_password"
	)

	var clientAddr string
	if env := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_CH_ADDR")); env != "" {
		// Escape hatch for flaky-testcontainers hosts: point at a ClickHouse on a
		// stable address (e.g. a --network host container at 127.0.0.1:9000) with
		// the same user/db/pass below.
		clientAddr = env
	} else {
		container, err := chmod.Run(ctx,
			"clickhouse/clickhouse-server:25.8-alpine",
			chmod.WithUsername(dbUser),
			chmod.WithPassword(dbPass),
			chmod.WithDatabase(dbName),
			testcontainers.WithWaitStrategy(
				wait.ForListeningPort(nat.Port("9000/tcp")).
					WithStartupTimeout(180*time.Second).
					WithPollInterval(time.Second),
			),
		)
		if err != nil {
			t.Fatalf("start clickhouse container: %v", err)
		}
		t.Cleanup(func() { _ = container.Terminate(ctx) })
		host, err := container.Host(ctx)
		if err != nil {
			t.Fatalf("container host: %v", err)
		}
		nativePort, err := container.MappedPort(ctx, "9000")
		if err != nil {
			t.Fatalf("mapped native port: %v", err)
		}
		clientAddr = fmt.Sprintf("%s:%s", host, nativePort.Port())
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{clientAddr},
		Auth: clickhouse.Auth{Database: dbName, Username: dbUser, Password: dbPass},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer conn.Close()
	// The container's entrypoint restarts ClickHouse after creating the user/db,
	// so the native port may accept TCP slightly before the server is query-ready.
	// Retry the ping for up to ~60s.
	var pingErr error
	for i := 0; i < 60; i++ {
		if pingErr = conn.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if pingErr != nil {
		t.Fatalf("ping clickhouse: %v", pingErr)
	}

	createMinimalDailyMetrics(t, ctx, conn)

	tenantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	tenantB := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))

	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	// Tenant A: 1000 sub revenue, 2 new subs, processor "ccbill".
	seedDailyMetric(t, ctx, conn, day, "usd", tenantA, dailySeed{
		subRevenue: 1000, newSubs: 2, paymentsSuccessful: 2,
		activeEnd: 5, cancellationsUser: 1, processor: "ccbill",
	})
	// Tenant B: deliberately different magnitudes so any leak is obvious.
	seedDailyMetric(t, ctx, conn, day, "usd", tenantB, dailySeed{
		subRevenue: 9999, newSubs: 7, paymentsSuccessful: 7,
		activeEnd: 42, cancellationsUser: 3, processor: "nmi",
	})

	svc := &AdminMetricsService{cfg: &config.ClickHouseConfig{ClientAddr: clientAddr, Database: dbName, Username: dbUser, Password: dbPass}}

	rng := MetricsDateRange{Start: day.Add(-24 * time.Hour), End: day.Add(48 * time.Hour)}
	ctxA := merchant.WithID(ctx, tenantA)

	// --- Summary ---
	summary, err := svc.GetSummary(ctxA, rng, "usd")
	if err != nil {
		t.Fatalf("GetSummary(A): %v", err)
	}
	if len(summary) != 1 {
		t.Fatalf("GetSummary(A) rows = %d, want 1", len(summary))
	}
	if summary[0].SubscriptionRevenue != 1000 {
		t.Fatalf("GetSummary(A) subscription_revenue = %d, want 1000 (B's 9999 must NOT be included)", summary[0].SubscriptionRevenue)
	}
	if summary[0].NewSubscriptions != 2 {
		t.Fatalf("GetSummary(A) new_subscriptions = %d, want 2 (tenant B leaked)", summary[0].NewSubscriptions)
	}

	// --- Revenue series ---
	rev, err := svc.GetRevenueSeries(ctxA, rng, "day", "usd")
	if err != nil {
		t.Fatalf("GetRevenueSeries(A): %v", err)
	}
	var revSub int64
	for _, r := range rev {
		revSub += r.Totals.SubscriptionRevenue
	}
	if revSub != 1000 {
		t.Fatalf("GetRevenueSeries(A) subscription revenue = %d, want 1000 (tenant B leaked)", revSub)
	}

	// --- Subscription series ---
	subs, err := svc.GetSubscriptionSeries(ctxA, rng, "day", "usd")
	if err != nil {
		t.Fatalf("GetSubscriptionSeries(A): %v", err)
	}
	var newSubs int
	for _, s := range subs {
		newSubs += s.Totals.NewSubscriptions
	}
	if newSubs != 2 {
		t.Fatalf("GetSubscriptionSeries(A) new subs = %d, want 2 (tenant B leaked)", newSubs)
	}

	// --- Processor metrics ---
	procs, err := svc.GetProcessorMetrics(ctxA, rng, "usd")
	if err != nil {
		t.Fatalf("GetProcessorMetrics(A): %v", err)
	}
	for _, pr := range procs {
		for _, p := range pr.Processors {
			if p.Processor == "nmi" {
				t.Fatalf("GetProcessorMetrics(A) leaked tenant B processor 'nmi'")
			}
		}
	}

	// --- Churn ---
	churn, err := svc.GetChurn(ctxA, rng, "usd")
	if err != nil {
		t.Fatalf("GetChurn(A): %v", err)
	}
	for _, c := range churn {
		for _, rc := range c.CancellationReasons {
			if rc.Reason == "user" && rc.Count != 1 {
				t.Fatalf("GetChurn(A) user cancellations = %d, want 1 (tenant B's 3 leaked)", rc.Count)
			}
		}
	}

	// --- Tenant B sees only its own data (symmetry) ---
	ctxB := merchant.WithID(ctx, tenantB)
	summaryB, err := svc.GetSummary(ctxB, rng, "usd")
	if err != nil {
		t.Fatalf("GetSummary(B): %v", err)
	}
	if len(summaryB) != 1 || summaryB[0].SubscriptionRevenue != 9999 {
		t.Fatalf("GetSummary(B) = %+v, want only tenant B (9999)", summaryB)
	}

	// --- Platform-superadmin cross-tenant path sees BOTH ---
	crossSummary, err := svc.GetSummaryCrossTenant(ctx, rng, "usd")
	if err != nil {
		t.Fatalf("GetSummaryCrossTenant: %v", err)
	}
	if len(crossSummary) != 1 {
		t.Fatalf("GetSummaryCrossTenant rows = %d, want 1 aggregated currency row", len(crossSummary))
	}
	if crossSummary[0].SubscriptionRevenue != 1000+9999 {
		t.Fatalf("GetSummaryCrossTenant subscription_revenue = %d, want %d (both tenants summed)", crossSummary[0].SubscriptionRevenue, 1000+9999)
	}
}

// createMinimalDailyMetrics creates a single-node (non-replicated) daily_metrics
// table with the columns the AdminMetricsService queries read, including
// tenant_id. We use a plain MergeTree (no Keeper) so the test runs against a
// standalone container; production uses the Replicated engine from 001_schema.up.sql.
func createMinimalDailyMetrics(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	if err := conn.Exec(ctx, `DROP TABLE IF EXISTS daily_metrics`); err != nil {
		t.Fatalf("drop daily_metrics: %v", err)
	}
	ddl := `
CREATE TABLE daily_metrics (
    snapshot_date Date,
    currency LowCardinality(String),
    tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001',
    subscription_revenue_cents Int64,
    one_time_revenue_cents Int64,
    refunds_cents Int64,
    chargebacks_cents Int64,
    total_revenue_cents Int64,
    total_revenue_net_cents Int64,
    payments_successful Int64,
    payments_failed Int64,
    avg_payment_amount_cents Int64,
    new_subscriptions Int64,
    scheduled_starts Nullable(Int64),
    cancellations_user Int64,
    cancellations_merchant Int64,
    cancellations_expired Int64,
    cancellations_chargeback Int64,
    reactivations Int64,
    active_count_end Int64,
    past_due_count_end Int64,
    pending_count_end Int64,
    mrr_cents Int64,
    entitlements_granted Nullable(Int64),
    processor Nested (
        name String,
        active_subscriptions Int64,
        new_subscriptions Int64,
        cancellations Int64,
        revenue_total_cents Int64,
        revenue_subscription_cents Int64,
        revenue_one_time_cents Int64,
        revenue_refunds_cents Int64,
        revenue_chargebacks_cents Int64,
        payments_successful Int64,
        payments_failed Int64
    ),
    created_at DateTime('UTC') DEFAULT now()
) ENGINE = MergeTree()
ORDER BY (snapshot_date, currency, tenant_id)`
	if err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("create daily_metrics: %v", err)
	}
}

type dailySeed struct {
	subRevenue         int64
	newSubs            int64
	paymentsSuccessful int64
	activeEnd          int64
	cancellationsUser  int64
	processor          string
}

func seedDailyMetric(t *testing.T, ctx context.Context, conn driver.Conn, day time.Time, currency string, tenantID merchant.ID, s dailySeed) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO daily_metrics (
        snapshot_date, currency, tenant_id,
        subscription_revenue_cents, one_time_revenue_cents, refunds_cents, chargebacks_cents,
        total_revenue_cents, total_revenue_net_cents,
        payments_successful, payments_failed, avg_payment_amount_cents,
        new_subscriptions, scheduled_starts,
        cancellations_user, cancellations_merchant, cancellations_expired, cancellations_chargeback,
        reactivations, active_count_end, past_due_count_end, pending_count_end, mrr_cents, entitlements_granted,
        processor.name, processor.active_subscriptions, processor.new_subscriptions, processor.cancellations,
        processor.revenue_total_cents, processor.revenue_subscription_cents, processor.revenue_one_time_cents,
        processor.revenue_refunds_cents, processor.revenue_chargebacks_cents,
        processor.payments_successful, processor.payments_failed
    ) VALUES`)
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	scheduled := int64(0)
	ent := int64(0)
	if err := batch.Append(
		day, currency, tenantID.String(),
		s.subRevenue, int64(0), int64(0), int64(0),
		s.subRevenue, s.subRevenue,
		s.paymentsSuccessful, int64(0), s.subRevenue,
		s.newSubs, &scheduled,
		s.cancellationsUser, int64(0), int64(0), int64(0),
		int64(0), s.activeEnd, int64(0), int64(0), int64(0), &ent,
		[]string{s.processor}, []int64{s.activeEnd}, []int64{s.newSubs}, []int64{s.cancellationsUser},
		[]int64{s.subRevenue}, []int64{s.subRevenue}, []int64{0},
		[]int64{0}, []int64{0},
		[]int64{s.paymentsSuccessful}, []int64{0},
	); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send batch: %v", err)
	}
}
