//go:build integration

package critical_test

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestBillingImportBounds(t *testing.T) {
	t.Run("OR-J21-S61/a-line-whose-list-total-overflows-is-refused", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "seat", DisplayName: "Seat", Entitlements: []string{"seat:owned"}})
		require.NoError(t, err)
		// $10M a seat: the list total of MaxInt32 seats is far past int64 micros,
		// while the declared line ($0.01 a seat) is exact.
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "seat", UnitAmount: 10_000_000_000_000, Currency: "USD"})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		book := billing.DeclaredBilling{AsOf: a.clock.Now(), DefaultPSP: billing.PSPRef{Key: "stripe"}, Customers: []billing.DeclaredCustomer{{Customer: customer}}, Purchases: []billing.DeclaredPurchase{{
			SourceID: "seat-overflow", Customer: customer, Rail: "stripe", TransactionID: "ch_seat_overflow", Currency: "USD",
			Amount: 21_474_836_470_000, PaidAt: a.clock.Now().Add(-time.Hour),
			Lines: []billing.DeclaredOrderLine{{Price: price.ID, Quantity: ptr(math.MaxInt32), UnitAmount: 10_000}},
		}}}
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Equal(t, []string{"seat-overflow"}, imported.Blocked)
		require.Contains(t, imported.Reasons["seat-overflow"], "overflows")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, payments.Items, "a refused purchase records no payment")
	})
}
