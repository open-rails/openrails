//go:build integration

package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogAuthoredHistoryUsesDatabaseClockAndPreservesImports(t *testing.T) {
	s, ctx := applicationService(t)
	application := applicationParams(t, s, ctx)
	application.Products = []openrails.CatalogApplyProduct{applicationProduct("clock")}
	_, err := s.ApplyCatalog(ctx, application)
	require.NoError(t, err)
	price, err := s.GetPriceByKey(ctx, "clock-price")
	require.NoError(t, err)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	imported := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	prices := catalog.NewPriceService(s.catalogDatabase())
	require.NoError(t, prices.RecordKeyMovement(ctx, mid.UUID(), price.ID.UUID(), "clock-price", imported))
	var lower, upper time.Time
	require.NoError(t, s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// now() would use transaction start and predate this lower bound. Ordinary
		// movement time must use the database's current clock, like retirement.
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&lower); err != nil {
			return err
		}
		txPrices := catalog.NewPriceService(s.catalogDatabase().NewWithPgxTx(tx))
		if err := txPrices.RecordAuthoredKeyMovement(ctx, mid.UUID(), price.ID.UUID(), "clock-price"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&upper)
	}))
	history, err := prices.ListKeyMovements(ctx, mid.UUID(), "clock-price")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(history), 3)
	require.False(t, history[0].EffectiveAt.Before(lower))
	require.False(t, history[0].EffectiveAt.After(upper))
	require.Equal(t, imported, history[len(history)-1].EffectiveAt.UTC(), "explicit import timestamps are not rewritten")
}
