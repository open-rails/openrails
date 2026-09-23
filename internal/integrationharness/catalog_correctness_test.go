//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	service "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Exercise the ordinary authenticated HTTP handler and exported Go facade on
// separate PostgreSQL sessions. Holding the product lock lets both calls reach
// their catalog lock before either can commit: the old read/full-write lost one patch.
func TestCatalogDisjointPatchesAndClearSemantics(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	owned := surface.ProvisionOwnedMerchant("catalog" + uuid.NewString()[:8])
	ctx = merchant.WithID(ctx, owned.MerchantID)
	token := surface.MintAPIKey(owned.MerchantSlug, "patch-"+uuid.NewString(), []string{
		controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate,
	})
	svc, err := service.New(surface.App().Runtime)
	require.NoError(t, err)
	for _, transport := range []string{"Go", "HTTP"} {
		t.Run(transport, func(t *testing.T) {
			group, desc := "old-"+uuid.NewString(), "original"
			p, err := svc.CreateProduct(ctx, service.CreateProductRequest{
				Key: "patch-" + uuid.NewString(), DisplayName: "Original", Description: desc,
				TierGroup: &group, EntitlementsSpec: map[string]*int{"old": nil},
			})
			require.NoError(t, err)
			patch := func(req service.UpdateProductRequest) error {
				if transport == "Go" {
					_, err := svc.UpdateProduct(ctx, p.ID, req)
					return err
				}
				payload, err := json.Marshal(req)
				if err != nil {
					return err
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPatch, surface.BaseURL+"/v1/merchant/catalog/products/"+openrails.ProductID(p.ID).String(), bytes.NewReader(payload))
				if err != nil {
					return err
				}
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					return err
				}
				if response.StatusCode != http.StatusOK {
					return fmt.Errorf("status %d: %s", response.StatusCode, body)
				}
				return nil
			}
			tx, err := h.MerchantPool(owned.MerchantID.UUID()).Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, `SELECT id FROM billing.products WHERE id=$1 FOR UPDATE`, p.ID)
			require.NoError(t, err)
			name, archived, rank, nextGroup := "Renamed", true, 7, "new-"+uuid.NewString()
			results := make(chan error, 2)
			go func() { results <- patch(service.UpdateProductRequest{DisplayName: &name, SkipRailSync: true}) }()
			go func() {
				results <- patch(service.UpdateProductRequest{Archived: &archived, TierRank: &rank, TierGroup: &nextGroup,
					SetTierGroup: true, SetEntitlements: true, EntitlementsSpec: map[string]*int{"new": nil}, SkipRailSync: true})
			}()
			require.Eventually(t, func() bool {
				var n int
				err := h.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
                    AND wait_event_type='Lock' AND (query LIKE '%UPDATE billing.products%' OR query LIKE '%LockCatalogRevision%')`).Scan(&n)
				return err == nil && n >= 2
			}, 10*time.Second, 20*time.Millisecond, "both public patches must block on the product or merchant catalog lock")
			require.NoError(t, tx.Commit(ctx))
			require.NoError(t, <-results)
			require.NoError(t, <-results)
			got, err := svc.GetProduct(ctx, p.ID)
			require.NoError(t, err)
			require.Equal(t, name, got.DisplayName)
			require.True(t, got.Archived)
			require.Equal(t, rank, got.TierRank)
			require.Equal(t, &nextGroup, got.TierGroup)
			require.Equal(t, map[string]*int{"new": nil}, got.EntitlementsSpec)

			// JSON null without Set flags is omission in either client.
			var omitted service.UpdateProductRequest
			require.NoError(t, json.Unmarshal([]byte(`{"description":null,"tier_group":null,"entitlements_spec":null}`), &omitted))
			omitted.SkipRailSync = true
			require.NoError(t, patch(omitted))
			got, err = svc.GetProduct(ctx, p.ID)
			require.NoError(t, err)
			require.Equal(t, desc, got.Description)
			require.Equal(t, &nextGroup, got.TierGroup)
			empty := ""
			require.NoError(t, patch(service.UpdateProductRequest{Description: &empty, SetTierGroup: true, SetEntitlements: true, SkipRailSync: true}))
			got, err = svc.GetProduct(ctx, p.ID)
			require.NoError(t, err)
			require.Empty(t, got.Description)
			require.Nil(t, got.TierGroup)
			require.Empty(t, got.EntitlementsSpec)
			var cleared bool
			require.NoError(t, h.MerchantPool(owned.MerchantID.UUID()).QueryRow(ctx,
				`SELECT description IS NULL AND tier_group IS NULL AND entitlements_spec IS NULL FROM billing.products WHERE id=$1`, p.ID).Scan(&cleared))
			require.True(t, cleared)
		})
	}
}

func TestCatalogTierRegroupConflictsThroughPublicSurface(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	owned := surface.ProvisionOwnedMerchant("catalog" + uuid.NewString()[:8])
	ctx = merchant.WithID(ctx, owned.MerchantID)
	svc, err := service.New(surface.App().Runtime)
	require.NoError(t, err)
	token := surface.MintAPIKey(owned.MerchantSlug, "regroup-"+uuid.NewString(), []string{controlplane.PermMerchantCatalogUpdate})
	pool := h.MerchantPool(owned.MerchantID.UUID())
	mid := owned.MerchantID.UUID()
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid, "stripe")
	for _, status := range []string{"active", "pending", "past_due", "unknown"} {
		t.Run(status, func(t *testing.T) {
			group, next := "tier-"+uuid.NewString(), "new-"+uuid.NewString()
			p, err := svc.CreateProduct(ctx, service.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Tier", TierGroup: &group})
			require.NoError(t, err)
			customer := uuid.New()
			_, err = pool.Exec(ctx, `INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, customer, mid)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(merchant_id,customer_id,product_id,status,rail,psp_id,current_period_ends_at)
                VALUES($1,$2,$3,$4,'stripe',$5,now()+interval '1 day')`, mid, customer, p.ID, status, psp)
			require.NoError(t, err)
			_, err = svc.UpdateProduct(ctx, p.ID, service.UpdateProductRequest{SetTierGroup: true, TierGroup: &next})
			require.ErrorIs(t, err, service.ErrProductTierGroupInUse)
			code, body := requestJSON(t, http.MethodPatch, surface.BaseURL+"/v1/merchant/catalog/products/"+openrails.ProductID(p.ID).String(), token,
				service.UpdateProductRequest{SetTierGroup: true, TierGroup: &next})
			require.Equal(t, http.StatusConflict, code, string(body))
			require.NotContains(t, string(body), "SQLSTATE")
			got, err := svc.GetProduct(ctx, p.ID)
			require.NoError(t, err)
			require.Equal(t, &group, got.TierGroup)
			// past_due and unknown reserve the same group just like pending/active.
			second, err := svc.CreateProduct(ctx, service.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Other", TierGroup: &group})
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(merchant_id,customer_id,product_id,status,rail,psp_id)
                VALUES($1,$2,$3,'pending','stripe',$4)`, mid, customer, second.ID, psp)
			var pe interface{ SQLState() string }
			require.ErrorAs(t, err, &pe)
			require.Equal(t, "23505", pe.SQLState())
		})
	}
}
