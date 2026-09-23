//go:build integration

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogApplicationParentActivationVerifiesNewlyAvailableOffers(t *testing.T) {
	for _, mode := range []string{"named", "omitted", "omitted-pruned", "named-retired"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := applicationService(t)
			mid, err := merchant.Require(ctx)
			require.NoError(t, err)
			product, err := s.CreateProduct(ctx, CreateProductRequest{Key: "retired-parent", DisplayName: "Retired parent", Archived: true})
			require.NoError(t, err)
			hours := 720
			price, err := s.CreatePrice(ctx, CreatePriceRequest{ProductID: product.ID, Key: "live-child", Currency: "USD", UnitAmount: 1000000, AutoRenew: true, AccessDurationHours: &hours})
			require.NoError(t, err)
			require.False(t, price.Archived)
			owner := dbtest.SharedSuperuserPGXPool(t)
			psp := uuid.New()
			accountID := "parent-" + mid.String()
			_, err = owner.Exec(ctx, "INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id) VALUES($1,$2,'mobius','nmi','live',$3)", psp, mid.UUID(), accountID)
			require.NoError(t, err)
			_, err = owner.Exec(ctx, `INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,'parent-plan','{"provider":"mobius"}')`, mid.UUID(), price.ID.UUID(), psp)
			require.NoError(t, err)
			reads := 0
			amount := "2.00"
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/plans/parent-plan", r.URL.Path)
				reads++
				fmt.Fprintf(w, `{"object":"plan","id":"parent-plan","plan_amount":%q,"day_frequency":"30","plan_payments":"0"}`, amount)
			}))
			t.Cleanup(gateway.Close)
			adapter := newMobiusAdapterWithServer(t, gateway.URL)
			adapter.svc.rt.RailConfigs = railresolve.FixedSet{"mobius": {Rail: models.RailNMI, AccountID: accountID, NMI: &config.NMIRailConfig{SecurityKey: "test-parent-key"}}}
			verify := func(ctx context.Context, key, rail, selectedAccount, productKey string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
				require.False(t, req.Archived, "newly available offer needs active-reference validation")
				return verifyNMICatalogReference(ctx, adapter, selectedAccount, req, link)
			}
			application := applicationParams(t, s, ctx)
			entry := openrails.CatalogApplyProduct{Key: product.Key, Archived: openrails.CatalogValue(false)}
			if mode == "named" {
				entry.Prices = []openrails.CatalogApplyPrice{{Key: price.Key}}
			}
			if mode == "named-retired" {
				entry.Prices = []openrails.CatalogApplyPrice{{Key: price.Key, Archived: openrails.CatalogValue(true)}}
			}
			application.Products = []openrails.CatalogApplyProduct{entry}
			application.Prune = mode == "omitted-pruned"
			before := *application.ExpectedRevision
			receipt, err := s.applyCatalog(ctx, application, verify)
			if mode == "omitted-pruned" || mode == "named-retired" {
				require.NoError(t, err)
				require.Zero(t, reads, "offers retired by this application are not newly offered")
				current, e := s.GetPrice(ctx, price.ID)
				require.NoError(t, e)
				require.True(t, current.Archived)
				return
			}
			require.Error(t, err, "parent activation cannot bypass verification of existing live child references")
			require.Equal(t, 1, reads)
			current, e := s.GetProduct(ctx, product.ID)
			require.NoError(t, e)
			require.True(t, current.Archived)
			revision, e := s.CatalogRevision(ctx)
			require.NoError(t, e)
			require.Equal(t, before, revision)
			// The failed application ID is still available; correct remote terms allow
			// the identical declaration to commit without rewriting the child price.
			amount = "1.00"
			receipt, err = s.applyCatalog(ctx, application, verify)
			require.NoError(t, err)
			require.Equal(t, 2, reads)
			require.Zero(t, receipt.PricesChanged)
			current, e = s.GetProduct(ctx, product.ID)
			require.NoError(t, e)
			require.False(t, current.Archived)
			child, e := s.GetPrice(ctx, price.ID)
			require.NoError(t, e)
			require.False(t, child.Archived)
			require.Equal(t, price.ID, child.ID)
			replay, e := s.applyCatalog(ctx, application, verify)
			require.NoError(t, e)
			require.True(t, replay.Replayed)
			require.Equal(t, 2, reads)
		})
	}
}
