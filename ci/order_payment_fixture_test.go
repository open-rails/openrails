//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/stretchr/testify/require"
)

// recordFundingOrder keeps accounting tests on the same order/payment contract
// as production without also granting the credit being tested.
func recordFundingOrder(t *testing.T, ctx context.Context, database *db.DB, p *models.Payment, price, product uuid.UUID) uuid.UUID {
	t.Helper()
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	var line uuid.UUID
	require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := database.NewWithPgxTx(tx)
		r, err := observed.Record(ctx, bound, observed.Bill{PSPID: *p.PspID, CustomerID: p.CustomerID,
			Owner: observed.OwnerEngine, Kind: "purchase", SourceKind: "accounting_fixture", SourceKey: p.TransactionID,
			Currency: p.Currency, Total: p.Amount, Payment: p, Settled: true, SettlementSource: "cash",
			Lines: []observed.Line{{PriceID: &price, ProductID: &product, Description: "Purchased credit", UnitAmount: p.Amount, Amount: p.Amount}},
		}, p.PurchasedAt)
		if err != nil {
			return err
		}
		*p = *r.Payment
		lines, err := bound.Gen(ctx).ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: mid.UUID(), OrderIds: []uuid.UUID{r.Order.ID}})
		if err != nil {
			return err
		}
		require.Len(t, lines, 1)
		line = lines[0].ID
		return nil
	}))
	return line
}
