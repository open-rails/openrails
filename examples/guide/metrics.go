package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Revenue reads net cash and MRR by month for the last year.
func Revenue(ctx context.Context, shop *openrails.Client) (*billing.MetricsResult, error) {
	// guide:metrics
	return shop.QueryMetrics(ctx, billing.QueryMetricsParams{
		Measures: []string{"cash", "mrr"},
		By:       []string{"time"},
		Grain:    "month",
		Range:    &billing.MetricsRange{Last: "12m"},
	})
	// guide:end
}
