//go:build integration

package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestSolanaCatalog(t *testing.T) {
	t.Run("OR-J37-S71/a-price-created-with-its-product-publishes-its-plan-outside-the-catalog-lock", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		defer gate.unblock()
		s.wire.mu.Lock()
		s.wire.gate = gate
		s.wire.mu.Unlock()
		type created struct {
			price *billing.Price
			err   error
		}
		done := make(chan created, 1)
		go func() {
			price, err := s.alpha.CreatePrice(context.Background(), billing.CreatePriceParams{ProductData: &billing.CreatePriceProduct{Key: "chain-inline", DisplayName: "Chain inline"},
				Key: "chain-inline-weekly", UnitAmount: 10_000_000, Currency: "USD", SellVia: []catalog.SellVia{catalog.SellViaSolana}, BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
			done <- created{price, err}
		}()
		select {
		case <-gate.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the plan's publication never reached the chain")
		}
		<-s.wire.signed
		// While the publication waits on the chain, another catalog edit commits:
		// neither a transaction nor the catalog lock is held across it.
		edited := make(chan error, 1)
		go func() {
			_, err := s.alpha.CreateProduct(context.Background(), billing.CreateProductParams{Key: "chain-other", DisplayName: "Chain other"})
			edited <- err
		}()
		select {
		case err := <-edited:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("a catalog edit waited for a Solana plan's publication")
		}
		gate.unblock()
		var result created
		select {
		case result = <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the price was never created")
		}
		require.NoError(t, result.err)
		require.NotNil(t, result.price.SolanaPlan, "the published plan is bound to the price")
		product, err := s.alpha.GetProduct(t.Context(), result.price.ProductID)
		require.NoError(t, err)
		require.Equal(t, "chain-inline", product.Key)
		again, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductData: &billing.CreatePriceProduct{Key: "chain-inline", DisplayName: "Chain inline"},
			Key: "chain-inline-weekly", UnitAmount: 10_000_000, Currency: "USD", SellVia: []catalog.SellVia{catalog.SellViaSolana}, BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
		require.NoError(t, err)
		require.Equal(t, result.price.ID, again.ID)
		require.Equal(t, *result.price.SolanaPlan, *again.SolanaPlan, "a retry binds the same plan")
	})
}
