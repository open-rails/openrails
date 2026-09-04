//go:build integration

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	manifest "github.com/open-rails/openrails/pkg/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/stretchr/testify/require"
)

func catalogPageRequest[T any](t *testing.T, fx *findingsFixture, handler func(*httprequest.Request), query string) billingservice.CatalogPage[T] {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog?"+query, nil).WithContext(fx.ctx), fx.rt))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page billingservice.CatalogPage[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	return page
}

func TestCatalogProductFilteringAndEffectivePagination(t *testing.T) {
	fx := newFindingsFixture(t)
	fx.rt.MoneyService = money.NewMoneyService(fx.dbi, fx.rt.Clock)
	fx.rt.ProductService = catalog.NewProductService(fx.dbi)
	fx.rt.PriceService = catalog.NewPriceService(fx.dbi)
	// A thousand newer unrelated records precede this sparse group. Timestamps
	// deliberately tie: the id tiebreaker must prevent repeated/skipped rows.
	fx.exec(`INSERT INTO openrails.products (id,merchant_id,key,display_name,tier_group,archived,created_at)
 SELECT uuidv7(),$1,'page-'||i,'Product '||i,CASE WHEN i<=1005 THEN 'target' ELSE 'other' END,
 i<=1005 AND i%20=0,CASE WHEN i<=1005 THEN timestamptz '2026-01-01' ELSE timestamptz '2026-02-01' END
 FROM generate_series(1,2105) i`, fx.merchant)
	foreign := newFindingsFixture(t)
	foreign.exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name,tier_group) VALUES(uuidv7(),$1,'foreign-target','Foreign','target')`, foreign.merchant)
	seen := map[uuid.UUID]bool{}
	for offset := 0; ; {
		page := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, fmt.Sprintf("tier_group=TARGET&limit=100&offset=%d", offset))
		require.EqualValues(t, 1005, page.Total)
		require.Equal(t, 100, page.Limit)
		require.Equal(t, offset, page.Offset)
		for _, p := range page.Items {
			require.Equal(t, "target", *p.TierGroup)
			require.False(t, seen[p.ID])
			seen[p.ID] = true
		}
		offset = page.Offset + page.Limit
		if int64(offset) >= page.Total {
			break
		}
		require.Len(t, page.Items, 100)
	}
	require.Len(t, seen, 1005)
	zero := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, "tier_group=target&limit=0")
	require.Equal(t, 100, zero.Limit)
	require.Len(t, zero.Items, 100)
	large := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, "tier_group=target&limit=5000")
	require.Equal(t, 1000, large.Limit)
	require.Len(t, large.Items, 1000)
	last := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, fmt.Sprintf("tier_group=target&limit=5000&offset=%d", large.Offset+large.Limit))
	require.Len(t, last.Items, 5)
	active := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, "tier_group=target&active_only=true&limit=5000")
	require.EqualValues(t, 955, active.Total)
	require.Len(t, active.Items, 955)
	for _, p := range active.Items {
		require.False(t, p.Archived)
	}
	absent := catalogPageRequest[billingservice.CatalogProduct](t, fx, AdminListProducts, "tier_group=absent")
	require.Zero(t, absent.Total)
	require.Empty(t, absent.Items)
	// Manifest pruning is another full-list consumer: a request cap is not the
	// end of a tier group, and the plan must see its thousand-and-first product.
	fx.exec(`UPDATE openrails.products SET archived=false WHERE tier_group='target'`)
	svc, err := billingservice.New(fx.rt)
	require.NoError(t, err)
	plan, err := manifest.PlanWithOptions(fx.ctx, svc, &manifest.Manifest{TierGroups: []manifest.TierGroup{{Key: "target"}}}, manifest.PlanOptions{ArchiveMissingProducts: true})
	require.NoError(t, err)
	require.Len(t, plan.Groups, 1)
	require.Len(t, plan.Groups[0].RemovedProducts, 1005)
}

func TestCatalogPricesPageAcross1000AndProductFilter(t *testing.T) {
	fx := newFindingsFixture(t)
	fx.rt.MoneyService = money.NewMoneyService(fx.dbi, fx.rt.Clock)
	fx.rt.ProductService = catalog.NewProductService(fx.dbi)
	fx.rt.PriceService = catalog.NewPriceService(fx.dbi)
	productID := uuid.New()
	fx.exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,'many-prices','Many prices')`, productID, fx.merchant)
	fx.exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,created_at)
 SELECT uuidv7(),$1,$2,'page-price-'||i,i*10000,'USD',timestamptz '2026-01-01' FROM generate_series(1,1001) i`, fx.merchant, productID)
	for _, filter := range []string{"", "product_id=" + productID.String() + "&"} {
		zero := catalogPageRequest[billingservice.CatalogPrice](t, fx, AdminListPrices, filter+"limit=0")
		require.Equal(t, 100, zero.Limit)
		require.Len(t, zero.Items, 100)
		first := catalogPageRequest[billingservice.CatalogPrice](t, fx, AdminListPrices, filter+"limit=5000")
		require.Equal(t, 1000, first.Limit)
		require.Len(t, first.Items, 1000)
		last := catalogPageRequest[billingservice.CatalogPrice](t, fx, AdminListPrices, fmt.Sprintf("%slimit=5000&offset=%d", filter, first.Offset+first.Limit))
		require.Equal(t, first.Total, last.Total)
		seen := map[uuid.UUID]bool{}
		for _, page := range []billingservice.CatalogPage[billingservice.CatalogPrice]{first, last} {
			for _, p := range page.Items {
				require.False(t, seen[p.ID])
				seen[p.ID] = true
				if filter != "" {
					require.Equal(t, productID, p.ProductID)
				}
			}
		}
		require.EqualValues(t, first.Total, len(seen))
		if filter != "" {
			require.EqualValues(t, 1001, first.Total)
			require.Len(t, last.Items, 1)
		}
	}
}
