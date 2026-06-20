package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
)

// MerchantMetric is the per-merchant aggregate row in the platform metrics view.
type MerchantMetric struct {
	MerchantID      string `json:"merchant_id"`
	Slug            string `json:"slug"`
	Status          string `json:"status"`
	ActiveSubs      int64  `json:"active_subscriptions"`
	RevenueMinor    int64  `json:"revenue_minor"`    // sum of completed payment amounts (minor units)
	WebhookFailures int64  `json:"webhook_failures"` // proxy: subscriptions in 'failed' status (see TODO)
}

// PlatformMetrics is the platform-wide aggregate (issue #226 platform metrics).
type PlatformMetrics struct {
	MerchantCount        int              `json:"merchant_count"`
	ActiveMerchantCount  int              `json:"active_merchant_count"`
	TotalActiveSubs      int64            `json:"total_active_subscriptions"`
	TotalRevenueMinor    int64            `json:"total_revenue_minor"`
	TotalWebhookFailures int64            `json:"total_webhook_failures"`
	Merchants            []MerchantMetric `json:"merchants"`
}

// Metrics aggregates platform-wide merchant metrics from existing billing tables
// (issue #226). It computes merchant count, and per-merchant active subscriptions,
// revenue, and webhook-failure counts.
type Metrics struct {
	pool *db.Pool
}

// NewMetrics builds the metrics aggregator over the control-plane pool.
func NewMetrics(pool *db.Pool) (*Metrics, error) {
	if pool == nil {
		return nil, errors.New("platform: metrics requires a pgx pool")
	}
	return &Metrics{pool: pool}, nil
}

// Compute returns the platform-wide metrics aggregate. It LEFT JOINs the tenant
// directory against per-merchant aggregates of openrails.subscriptions and
// openrails.payments (both carry merchant_id from the consolidated schema), so a merchant with
// no activity still appears with zeroes.
//
// NOTE on webhook failures: OpenRails does not keep a Postgres webhook-event
// table (delivery events live in ClickHouse), so this uses the count of
// subscriptions in the terminal 'failed' status as a same-DB proxy. TODO(#226):
// replace with a real per-merchant webhook delivery-failure count once a
// merchant-scoped webhook event table or a ClickHouse-backed aggregate is wired in.
func (m *Metrics) Compute(ctx context.Context) (*PlatformMetrics, error) {
	if m == nil || m.pool == nil {
		return nil, errors.New("platform: metrics not configured")
	}

	rows, err := m.pool.Query(ctx, `
		SELECT t.id::text,
		       t.slug,
		       t.status,
		       COALESCE(s.active_subs, 0)        AS active_subs,
		       COALESCE(p.revenue_minor, 0)      AS revenue_minor,
		       COALESCE(s.failed_subs, 0)        AS webhook_failures
		  FROM openrails.merchants t
		  LEFT JOIN (
		      SELECT merchant_id,
		             count(*) FILTER (WHERE status = 'active') AS active_subs,
		             count(*) FILTER (WHERE status = 'failed') AS failed_subs
		        FROM openrails.subscriptions
		       GROUP BY merchant_id
		  ) s ON s.merchant_id = t.id
		  LEFT JOIN (
		      SELECT merchant_id,
		             COALESCE(sum(amount), 0) AS revenue_minor
		        FROM openrails.payments
		       WHERE status = 'completed'
		       GROUP BY merchant_id
		  ) p ON p.merchant_id = t.id
		 WHERE t.deleted_at IS NULL
		 ORDER BY t.slug
	`)
	if err != nil {
		return nil, fmt.Errorf("platform: compute metrics: %w", err)
	}
	defer rows.Close()

	out := &PlatformMetrics{}
	out.Merchants, err = scanMerchantMetrics(rows)
	if err != nil {
		return nil, err
	}

	out.MerchantCount = len(out.Merchants)
	for _, tm := range out.Merchants {
		if tm.Status == "active" {
			out.ActiveMerchantCount++
		}
		out.TotalActiveSubs += tm.ActiveSubs
		out.TotalRevenueMinor += tm.RevenueMinor
		out.TotalWebhookFailures += tm.WebhookFailures
	}
	return out, nil
}

func scanMerchantMetrics(rows pgx.Rows) ([]MerchantMetric, error) {
	var out []MerchantMetric
	for rows.Next() {
		var tm MerchantMetric
		if err := rows.Scan(&tm.MerchantID, &tm.Slug, &tm.Status,
			&tm.ActiveSubs, &tm.RevenueMinor, &tm.WebhookFailures); err != nil {
			return nil, err
		}
		out = append(out, tm)
	}
	return out, rows.Err()
}
