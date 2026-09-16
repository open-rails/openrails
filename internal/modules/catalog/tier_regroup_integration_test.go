//go:build integration

package catalog

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestTierRegroupSerializesWithSubscriptionCreation(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	mid := dbtest.TestMerchantID.UUID()
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid, "stripe")
	for _, insertFirst := range []bool{true, false} {
		name := "regroup_first"
		if insertFirst {
			name = "enrollment_first"
		}
		t.Run(name, func(t *testing.T) {
			product, customer, sub := uuid.New(), uuid.New(), uuid.New()
			_, err := pool.Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name,tier_group) VALUES($1,$2,$3,'Product','before')`, product, mid, uuid.NewString())
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$2)`, customer, mid)
			require.NoError(t, err)
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(ctx)
			insert := `INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,rail,psp_id,status) VALUES($1,$2,$3,$4,'stripe',$5,'pending')`
			update := `UPDATE openrails.products SET tier_group='after' WHERE id=$1`
			result := make(chan error, 1)
			if insertFirst {
				_, err = tx.Exec(ctx, insert, sub, mid, customer, product, psp)
				require.NoError(t, err)
				go func() {
					_, err := NewProductService(dbi).UpdateDefinition(ctx, product, ProductDefinitionUpdateParams{SetTierGroup: true, TierGroup: ptr("after")})
					result <- err
				}()
			} else {
				_, err = tx.Exec(ctx, update, product)
				require.NoError(t, err)
				go func() { _, err := pool.Exec(ctx, insert, sub, mid, customer, product, psp); result <- err }()
			}
			// The second operation must remain blocked behind the first owner.
			select {
			case err := <-result:
				t.Fatalf("operation did not wait for product lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, tx.Commit(ctx))
			err = <-result
			if insertFirst {
				require.ErrorIs(t, err, ErrProductTierGroupInUse)
			} else {
				require.NoError(t, err)
			}
			var actual, cached string
			require.NoError(t, pool.QueryRow(ctx, `SELECT p.tier_group,s.tier_group FROM openrails.products p JOIN openrails.subscriptions s ON s.product_id=p.id WHERE s.id=$1`, sub).Scan(&actual, &cached))
			require.Equal(t, actual, cached)
			if insertFirst {
				require.Equal(t, "before", actual)
			} else {
				require.Equal(t, "after", actual)
			}
		})
	}
}

func ptr(s string) *string { return &s }
