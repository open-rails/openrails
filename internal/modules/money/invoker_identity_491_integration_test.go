//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #539: customer identity is merchant + stable host/AuthKit UUID subject.
// Issuer is audit metadata only; adding/removing/changing issuer URLs must not
// split one real customer.
func TestInvokerIdentityAndPayerNaturalKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	q := dbtest.Queries(pool)

	t.Run("subject_natural_key_is_idempotent_across_issuers", func(t *testing.T) {
		issuerA := "https://host-one.example"
		issuerB := "https://host-two.example"
		subjAID := uuid.New()
		subjBID := uuid.New()

		a1, err := q.EnsureCustomer(ctx, gen.EnsureCustomerParams{MerchantID: merchantID, Issuer: &issuerA, ID: subjAID})
		require.NoError(t, err)
		a2, err := q.EnsureCustomer(ctx, gen.EnsureCustomerParams{MerchantID: merchantID, Issuer: &issuerB, ID: subjAID})
		require.NoError(t, err)
		require.Equal(t, a1.ID, a2.ID, "same (merchant,subject) must survive issuer changes")

		b1, err := q.EnsureCustomer(ctx, gen.EnsureCustomerParams{MerchantID: merchantID, Issuer: &issuerA, ID: subjBID})
		require.NoError(t, err)
		require.NotEqual(t, a1.ID, b1.ID, "distinct subjects -> distinct customers")

		var issuer string
		require.NoError(t, pool.QueryRow(ctx, "SELECT issuer FROM billing.customers WHERE merchant_id=$1 AND id=$2", merchantID, subjAID).Scan(&issuer))
		require.Equal(t, issuerB, issuer)
	})
}
