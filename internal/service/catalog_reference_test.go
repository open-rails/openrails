package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestNMICatalogReferencePreflightRejectsMismatch(t *testing.T) {
	req := CreatePriceRequest{Currency: "USD", UnitAmount: 23_000_000, AccessDurationHours: intPtr(720), AutoRenew: true}
	for _, remote := range []string{nmiPlanJSON("known", "23.00", "0"), nmiPlanJSON("known", "23.00", "31"), nmiPlanJSON("known", "19.00", "30")} {
		srv, creates := fakeNMIPlans(t, map[string]string{"known": remote})
		_, err := verifyNMICatalogReference(nmiCatalogCtx(), newMobiusAdapterWithServer(srv.URL), "mobius", req, map[string]string{"plan_id": "known"})
		require.ErrorContains(t, err, "does not match", remote)
		require.Empty(t, *creates)
	}
}

// Reference preflight only GETs, and accepts the provider's literal minor
// units only when they equal the native amount exactly (no rounding, JPY is
// zero-decimal, integers beyond float precision stay exact).
func TestStripeCatalogReferencePreflight(t *testing.T) {
	serve := func(t *testing.T, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/prices/") {
				t.Errorf("reference verifier attempted %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	monthly := CreatePriceRequest{Currency: "USD", UnitAmount: 23_000_000, AccessDurationHours: intPtr(720), AutoRenew: true}
	adapter := newStripeAdapterWithServer(serve(t, `{"id":"price_existing","product":"prod_existing","unit_amount":2300,"currency":"usd","active":true,"recurring":{"interval":"month","interval_count":1}}`))
	out, err := verifyStripeCatalogReference(t.Context(), adapter, "", monthly, map[string]string{"price_id": "price_existing"})
	require.NoError(t, err)
	require.Equal(t, "prod_existing", out["product_id"])
	oneTime := monthly
	oneTime.AutoRenew = false
	_, err = verifyStripeCatalogReference(t.Context(), adapter, "", oneTime, map[string]string{"price_id": "price_existing"})
	require.ErrorContains(t, err, "recurring")
	_, err = verifyStripeCatalogReference(t.Context(), adapter, "", monthly, map[string]string{"lookup_key": "would-create"})
	require.ErrorContains(t, err, "existing Stripe price_id")

	for _, tc := range []struct {
		name, currency      string
		native, remoteMinor int64
		ok                  bool
	}{
		{"USD exact cents", "USD", 19_990_000, 1999, true},
		{"USD fractional cent", "USD", 19_995_000, 1999, false},
		{"JPY whole yen", "JPY", 5_000_000, 500, true},
		{"JPY fractional yen", "JPY", 5_000_001, 500, false},
		{"JPY cent assumption", "JPY", 5_000_000, 50000, false},
		{"beyond float precision", "USD", 9_007_199_254_750_000, 900_719_925_475, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"price_precision","product":"prod_precision","unit_amount":%d,"currency":%q,"active":true}`, tc.remoteMinor, strings.ToLower(tc.currency))
			_, err := verifyStripeCatalogReference(t.Context(), newStripeAdapterWithServer(serve(t, body)), "", CreatePriceRequest{Currency: tc.currency, UnitAmount: tc.native}, map[string]string{"price_id": "price_precision"})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// Creator-scoped callers may manage only their own catalog's ordinary fields:
// provider/entitlement/tier authority is refused before any side effect.
func TestCreatorCatalogRefusalsBeforeSideEffects(t *testing.T) {
	mid := merchant.ID(uuid.New())
	ctx, err := catalogscope.WithOwner(merchant.WithID(t.Context(), mid), catalogscope.Scope{MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "作者/opaque subject?x=1"})
	require.NoError(t, err)
	svc := &Service{} // unwired: any DB/provider access would panic
	tier, rank := "premium", 1
	product, price := openrails.ProductID(uuid.New()), openrails.PriceID(uuid.New())
	for name, call := range map[string]func() error{
		"create entitlements": func() error {
			_, err := svc.CreateProduct(ctx, CreateProductRequest{EntitlementsSpec: map[string]*int{}})
			return err
		},
		"create tier": func() error { _, err := svc.CreateProduct(ctx, CreateProductRequest{TierGroup: &tier}); return err },
		"patch entitlements": func() error {
			_, err := svc.UpdateProduct(ctx, product, UpdateProductRequest{SetEntitlements: true})
			return err
		},
		"patch tier rank": func() error {
			_, err := svc.UpdateProduct(ctx, product, UpdateProductRequest{TierRank: &rank})
			return err
		},
		"skip product sync": func() error {
			_, err := svc.UpdateProduct(ctx, product, UpdateProductRequest{SkipRailSync: true})
			return err
		},
		"choose PSP": func() error { _, err := svc.CreatePrice(ctx, CreatePriceRequest{PSPs: []string{"stripe"}}); return err },
		"attach PSP": func() error {
			_, err := svc.CreatePrice(ctx, CreatePriceRequest{PSPLinks: map[string]map[string]string{}})
			return err
		},
		"replace PSP": func() error {
			_, err := svc.UpdatePrice(ctx, price, UpdatePriceRequest{ReplacePSPLinks: true})
			return err
		},
		"skip price sync": func() error {
			_, err := svc.UpdatePrice(ctx, price, UpdatePriceRequest{SkipRailSync: true})
			return err
		},
		"verify provider":   func() error { _, err := svc.VerifyPriceSync(ctx, uuid.New()); return err },
		"reconcile price":   func() error { _, err := svc.ReconcilePrice(ctx, uuid.New(), ReconcileOptions{}); return err },
		"reconcile product": func() error { _, err := svc.ReconcileProduct(ctx, uuid.New(), ReconcileOptions{}); return err },
	} {
		require.ErrorIs(t, call(), catalog.ErrOwnerOperation, name)
	}
	_, err = svc.CreateProduct(ctx, CreateProductRequest{CatalogID: openrails.CatalogID(uuid.New())})
	require.ErrorIs(t, err, catalog.ErrOwnerScope, "a creator cannot target another catalog")
	_, err = svc.GetProduct(merchant.WithID(ctx, merchant.ID(uuid.New())), openrails.ProductID(uuid.New()))
	require.ErrorIs(t, err, catalog.ErrOwnerScope, "changing the merchant must not widen a captured owner scope")
}
