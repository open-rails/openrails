//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// One engine serves every live merchant of its directory through a handle
// each: the declared one by slug, any other by ID, all over the engine's one
// pool. A handle costs nothing to make; a merchant that is not live fails at
// the call.
func TestEngineMerchantHandles(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	slug := "handles-" + uuid.NewString()[:8]
	// The engine's own pool: what its handles' calls run on.
	pool, err := pgxpool.New(ctx, f.dsn(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug}}
	bill, err := openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })

	// A second merchant in the same directory, provisioned by an engine that
	// is gone before any handle is used.
	other := f.config()
	other.Merchants = map[string]openrails.MerchantDeclaration{slug + "-b": {DisplayName: "B"}}
	provisioner, err := openrails.New(ctx, other, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	theirs, err := provisioner.MerchantBySlug(slug + "-b")
	require.NoError(t, err)
	second := theirs.MerchantID()
	require.NoError(t, provisioner.Close(ctx))

	shop, err := bill.MerchantBySlug(" " + slug + " ")
	require.NoError(t, err)
	first := shop.MerchantID()
	require.False(t, first.IsZero())
	require.NotEqual(t, first, second)
	byID, err := bill.Merchant(first)
	require.NoError(t, err)
	elsewhere, err := bill.Merchant(second)
	require.NoError(t, err)

	// The declared merchant's handle reads and writes, and a handle by ID is
	// the same merchant.
	customer := billing.CustomerID(uuid.New())
	grant := func(c *openrails.Client, amount int64) {
		t.Helper()
		_, err := c.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "support", SourceID: "handles"}})
		require.NoError(t, err)
	}
	balance := func(c *openrails.Client) int64 {
		t.Helper()
		b, err := c.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		return b.BalanceAmount
	}
	grant(shop, 3_000_000)
	require.EqualValues(t, 3_000_000, balance(byID))
	_, err = byID.CreateProduct(ctx, billing.CreateProductParams{Key: "pro", DisplayName: "Pro"})
	require.NoError(t, err)

	// Another merchant's handle acts on that merchant alone, on the same
	// engine and pool: the engine that provisioned it is closed.
	before := pool.Stat().AcquireCount()
	grant(elsewhere, 7_000_000)
	require.EqualValues(t, 7_000_000, balance(elsewhere))
	require.EqualValues(t, 3_000_000, balance(shop), "the same customer id at another merchant is another customer")
	products, err := elsewhere.ListProducts(ctx, billing.ProductListParams{})
	require.NoError(t, err)
	require.Empty(t, products.Items, "the first merchant's product is not this one's")
	require.Greater(t, pool.Stat().AcquireCount(), before, "the second merchant's calls ran on the engine's pool")

	// Refused before any I/O: there is no context to do any with.
	_, err = bill.Merchant(billing.MerchantID{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	for _, undeclared := range []string{"", slug + "-b", "nobody", first.String()} {
		_, err = bill.MerchantBySlug(undeclared)
		require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "%q is not a slug Config declares", undeclared)
	}

	// An unknown merchant has a handle; its first call is refused.
	unknown, err := bill.Merchant(billing.MerchantID(uuid.New()))
	require.NoError(t, err)
	_, err = unknown.GetBalance(ctx, customer, "USD")
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved)
	require.ErrorIs(t, err, billing.ErrDenied)
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, billing.CodeMerchantUnresolved, refusal.Code)
	_, err = unknown.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1, Source: "support", SourceID: "unknown"}})
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "a write is refused as a read is")
	_, err = unknown.DeclarePSP(ctx, billing.PSPDeclaration{Key: "imported", Rail: billing.RailNMI, AccountID: "000000"})
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = unknown.GetProviderOperationTx(ctx, tx, "rental/1")
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "the host's transaction is bound to a live merchant only")

	// What only an engine's handle has acts on the handle's merchant.
	psp, err := elsewhere.DeclarePSP(ctx, billing.PSPDeclaration{Key: "imported", Rail: billing.RailNMI, AccountID: "000000"})
	require.NoError(t, err)
	listed, err := elsewhere.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1)
	require.Equal(t, psp.ID, listed.Items[0].ID)
	none, err := shop.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	require.Empty(t, none.Items)
}
