//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

// Exercise the producer, archive and reader together: rate-card monetary JSON
// is persisted using the same exact strings as the public wire contract.
func TestProducedRateCardsArchiveRoundTrip(t *testing.T) {
	source := archiveDB(t, "openrails")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	ctx := merchant.WithID(t.Context(), id)
	product := uuid.New()
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO openrails.products(merchant_id,id,key,display_name) VALUES($1,$2,'usage','Usage')`, id.UUID(), product)
		return err
	}))
	svc := money.NewMoneyService(source)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: "compute", EventType: "compute.usage", ValueProperty: "$.units", Aggregation: pricing.AggregationSum,
		Unit: "second", GroupBy: map[string]string{"size": "$.size"},
	}))
	for _, tc := range []struct {
		name  string
		price pricing.RatePrice
	}{
		{"plain", pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 9007199254740993, DivideBy: 1, MaximumAmount: math.MaxInt64}}},
		{"matrix", pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{DivideBy: 60, Round: pricing.RoundUp, MaximumAmount: math.MaxInt64, Matrix: &pricing.Matrix{Dimension: "size", Cells: map[string]pricing.MatrixCell{"large": {UnitAmount: 9007199254740993, MaximumAmount: math.MaxInt64, Included: 7}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The second pass updates the existing rate card, through the same
			// production operation used by the merchant API.
			require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
				ProductID: &product, MeterKey: "compute", Price: tc.price, Allowance: &pricing.Allowance{Included: 3},
			}))
			before, err := svc.GetUsageMeter(ctx, "compute")
			require.NoError(t, err)
			require.Equal(t, tc.price, before.DefaultRateCard.Price)
			var original bytes.Buffer
			require.NoError(t, Export(ctx, source, id, &original))
			target := archiveDB(t, "rate_archive_"+tc.name)
			provision(t, target, id)
			_, err = Restore(ctx, target, id, bytes.NewReader(original.Bytes()))
			require.NoError(t, err)
			after, err := money.NewMoneyService(target).GetUsageMeter(ctx, "compute")
			require.NoError(t, err)
			require.Equal(t, before.DefaultRateCard, after.DefaultRateCard)
			var restored bytes.Buffer
			require.NoError(t, Export(ctx, target, id, &restored))
			require.Equal(t, original.String(), restored.String(), "retained rate-card JSON and all domain IDs are unchanged")
		})
	}
}
