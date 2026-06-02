package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantMetric is the per-tenant aggregate row in the platform metrics view.
type TenantMetric struct {
	TenantID        string `json:"tenant_id"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	BillingTier     string `json:"billing_tier,omitempty"`
	ActiveSubs      int64  `json:"active_subscriptions"`
	RevenueMinor    int64  `json:"revenue_minor"`    // sum of completed payment amounts (minor units)
	WebhookFailures int64  `json:"webhook_failures"` // proxy: subscriptions in 'failed' status (see TODO)
}

// PlatformMetrics is the platform-wide aggregate (issue #226 platform metrics).
type PlatformMetrics struct {
	TenantCount          int            `json:"tenant_count"`
	ActiveTenantCount    int            `json:"active_tenant_count"`
	SuspendedTenantCount int            `json:"suspended_tenant_count"`
	TotalActiveSubs      int64          `json:"total_active_subscriptions"`
	TotalRevenueMinor    int64          `json:"total_revenue_minor"`
	TotalWebhookFailures int64          `json:"total_webhook_failures"`
	Tenants              []TenantMetric `json:"tenants"`
}

// Metrics aggregates platform-wide tenant metrics from existing billing tables
// (issue #226). It computes tenant count, and per-tenant active subscriptions,
// revenue, and webhook-failure counts.
type Metrics struct {
	pool *pgxpool.Pool
}

// NewMetrics builds the metrics aggregator over the control-plane pool.
func NewMetrics(pool *pgxpool.Pool) (*Metrics, error) {
	if pool == nil {
		return nil, errors.New("platform: metrics requires a pgx pool")
	}
	return &Metrics{pool: pool}, nil
}

// Compute returns the platform-wide metrics aggregate. It LEFT JOINs the tenant
// directory against per-tenant aggregates of billing.subscriptions and
// billing.payments (both carry tenant_id from migration 039), so a tenant with
// no activity still appears with zeroes.
//
// NOTE on webhook failures: OpenRails does not keep a Postgres webhook-event
// table (delivery events live in ClickHouse), so this uses the count of
// subscriptions in the terminal 'failed' status as a same-DB proxy. TODO(#226):
// replace with a real per-tenant webhook delivery-failure count once a
// tenant-scoped webhook event table or a ClickHouse-backed aggregate is wired in.
func (m *Metrics) Compute(ctx context.Context) (*PlatformMetrics, error) {
	if m == nil || m.pool == nil {
		return nil, errors.New("platform: metrics not configured")
	}

	rows, err := m.pool.Query(ctx, `
		SELECT t.id::text,
		       t.slug,
		       t.name,
		       t.status,
		       COALESCE(t.billing_tier, ''),
		       COALESCE(s.active_subs, 0)        AS active_subs,
		       COALESCE(p.revenue_minor, 0)      AS revenue_minor,
		       COALESCE(s.failed_subs, 0)        AS webhook_failures
		  FROM billing.tenants t
		  LEFT JOIN (
		      SELECT tenant_id,
		             count(*) FILTER (WHERE status = 'active') AS active_subs,
		             count(*) FILTER (WHERE status = 'failed') AS failed_subs
		        FROM billing.subscriptions
		       GROUP BY tenant_id
		  ) s ON s.tenant_id = t.id
		  LEFT JOIN (
		      SELECT tenant_id,
		             COALESCE(sum(amount), 0) AS revenue_minor
		        FROM billing.payments
		       WHERE status = 'completed'
		       GROUP BY tenant_id
		  ) p ON p.tenant_id = t.id
		 WHERE t.deleted_at IS NULL
		 ORDER BY t.slug
	`)
	if err != nil {
		return nil, fmt.Errorf("platform: compute metrics: %w", err)
	}
	defer rows.Close()

	out := &PlatformMetrics{}
	out.Tenants, err = scanTenantMetrics(rows)
	if err != nil {
		return nil, err
	}

	out.TenantCount = len(out.Tenants)
	for _, tm := range out.Tenants {
		switch tm.Status {
		case "active":
			out.ActiveTenantCount++
		case "suspended":
			out.SuspendedTenantCount++
		}
		out.TotalActiveSubs += tm.ActiveSubs
		out.TotalRevenueMinor += tm.RevenueMinor
		out.TotalWebhookFailures += tm.WebhookFailures
	}
	return out, nil
}

func scanTenantMetrics(rows pgx.Rows) ([]TenantMetric, error) {
	var out []TenantMetric
	for rows.Next() {
		var tm TenantMetric
		if err := rows.Scan(&tm.TenantID, &tm.Slug, &tm.Name, &tm.Status,
			&tm.BillingTier, &tm.ActiveSubs, &tm.RevenueMinor, &tm.WebhookFailures); err != nil {
			return nil, err
		}
		out = append(out, tm)
	}
	return out, rows.Err()
}
