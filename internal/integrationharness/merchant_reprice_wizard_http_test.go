//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// These tests drive the #777 console price-change wizard's backend contract
// end to end over real HTTP: the read-only affected-count preview and
// version-history endpoints added in this PR, plus the #773 bulk reprice /
// list / cancel surface the wizard rides directly. UI-level behavior (step
// defaults, the notice-window date gate, the review-in-words text) has NO
// automated coverage — web/admin has no test runner (tsc/eslint/vite build
// only) — so this file is the whole automated proof for the wizard's four
// spec cases; everything at the React layer needs manual/future e2e
// verification.

// seedRepriceSubscription inserts an active subscription pinned to priceID
// directly (no checkout rail needed for these read/schedule-path tests) —
// mirrors internal/modules/subscriptions/reprice_integration_test.go's own
// fixture idiom.
func seedRepriceSubscription(t *testing.T, ctx context.Context, h *Harness, productID, priceID uuid.UUID) uuid.UUID {
	t.Helper()
	pool := h.Pool()
	var customerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1,$2) RETURNING id`,
		dbtest.TestMerchantID.UUID(), "wizard-customer-"+uuid.NewString()).Scan(&customerID))

	now := time.Now().UTC()
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")
	var subID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, psp_id)
		VALUES ($1,$2,$3,$4,'active','nmi',$5,$6,$7,$8) RETURNING id`,
		dbtest.TestMerchantID.UUID(), customerID, productID, priceID, "wizard-rail-sub-"+uuid.NewString(), now, now.Add(30*24*time.Hour), pspID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.customers WHERE id = $1", customerID)
	})
	return subID
}

// TestStandaloneMerchantRepriceWizardIncreaseHTTP drives an INCREASE through
// the wizard's whole backend contract: preview the affected count BEFORE the
// price edit lands, bump the price under the same key (the #774 grandfather-
// by-default outcome — no reprice call needed for existing subscribers to
// stay put), confirm the version-history endpoint reflects both rows with
// dates, then explicitly migrate via the bulk endpoint and cancel before
// effective.
func TestStandaloneMerchantRepriceWizardIncreaseHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"reprice-wizard-"+uuid.NewString(),
		[]string{
			controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate,
			controlplane.PermMerchantSubscriptionsRead, controlplane.PermMerchantSubscriptionsUpdate,
			controlplane.PermMerchantSettingsUpdate,
		},
	)

	// #781: pin this merchant's notice window explicitly via the real settings
	// endpoint rather than relying on the "unset ⇒ default 30d" fallback — the
	// merchant row is shared across every integration package in this suite
	// (dbtest.TestMerchantID), so a deterministic explicit value keeps this
	// test's notice-window assertions below independent of what any other
	// package's fixtures last wrote to it.
	settingsStatus, settingsBody := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/settings", token, map[string]any{
		"reprice_notice_window_days": 30,
	})
	require.Equal(t, http.StatusOK, settingsStatus, string(settingsBody))

	productKey := "wizard-inc-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	priceKey := productKey + "-monthly"
	publish := func(amount int64) {
		t.Helper()
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": catalog.Manifest{
				Version: catalog.SupportedVersion,
				Products: []catalog.Product{{
					Key:         productKey,
					DisplayName: "Wizard Increase Product",
					Prices: []catalog.Price{{
						UnitAmount: amount, Currency: "USD", Duration: "30d", AutoRenew: true,
					}},
				}},
			},
			"insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))
	}
	getByKey := func() billingservice.CatalogPrice {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+priceKey, token, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var p billingservice.CatalogPrice
		require.NoError(t, json.Unmarshal(body, &p))
		return p
	}

	// v1 at $10/mo.
	publish(1000000)
	v1 := getByKey()
	require.EqualValues(t, 1000000, v1.UnitAmount)

	// One existing subscriber pinned to v1.
	seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID)

	// Step 2 preview, BEFORE the price edit: counts the whole chain (just v1
	// today), so it already reflects who would move once the bump lands.
	previewStatus, previewBody := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/catalog/reprice-all-prior-versions/preview?price_key="+priceKey, token, nil)
	require.Equal(t, http.StatusOK, previewStatus, string(previewBody))
	var preview subscriptions.RepricePreviewResult
	require.NoError(t, json.Unmarshal(previewBody, &preview))
	require.Equal(t, 1, preview.Matched, "preview counts the existing subscriber before the bump")
	require.Equal(t, v1.ID, preview.ToPriceID, "pre-bump, the current price IS the preview target")

	// Step 3 confirm: catalog price update (the increase) lands first...
	publish(1200000)
	v2 := getByKey()
	require.EqualValues(t, 1200000, v2.UnitAmount)
	require.NotEqual(t, v1.ID, v2.ID)

	// ...grandfather is the DEFAULT outcome on increase: without an explicit
	// migrate call, the existing subscriber's price_id is untouched.
	var pinnedPriceID uuid.UUID
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT price_id FROM openrails.subscriptions WHERE product_id = $1 AND status = 'active' LIMIT 1`, v1.ProductID).Scan(&pinnedPriceID))
	require.Equal(t, v1.ID, pinnedPriceID, "increase defaults to grandfather: subscriber stays on the old price")

	// Version-chain history, most-recent-first, resolved from the movement
	// log (not each row's own created_at).
	histStatus, histBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+priceKey+"/history", token, nil)
	require.Equal(t, http.StatusOK, histStatus, string(histBody))
	var hist struct {
		Items []billingservice.PriceKeyHistoryEntry `json:"items"`
	}
	require.NoError(t, json.Unmarshal(histBody, &hist))
	require.Len(t, hist.Items, 2)
	require.Equal(t, v2.ID, hist.Items[0].Price.ID, "most recent movement first")
	require.Equal(t, v1.ID, hist.Items[1].Price.ID)
	require.True(t, hist.Items[0].EffectiveAt.After(hist.Items[1].EffectiveAt) || hist.Items[0].EffectiveAt.Equal(hist.Items[1].EffectiveAt))

	// Now explicitly migrate (ending the grandfather window). #781: the
	// server now enforces a notice window for INCREASE reprices — an
	// effective_at only a day out is REFUSED (this used to be #777's
	// gap-proving assertion: pre-#781, the backend accepted it without
	// complaint). TestStandaloneMerchantRepriceNoticeWindowHTTP owns the full
	// notice-window matrix (refusal/decrease-exemption/acknowledge-override/
	// merchant-configured-window); here we just confirm the wizard's own flow
	// hits the gate, then proceed with a compliant date so the rest of this
	// end-to-end flow (batch listing, cancel) still exercises real data.
	shortNoticeStatus, shortNoticeBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/reprice-all-prior-versions", token, map[string]any{
		"price_key": priceKey, "effective_at": time.Now().UTC().Add(24 * time.Hour),
	})
	require.Equal(t, http.StatusCreated, shortNoticeStatus, string(shortNoticeBody), "bulk never aborts — the 1-day-out increase is SKIPPED, not a batch-level error")
	var shortNoticeResult subscriptions.RepriceBatchResult
	require.NoError(t, json.Unmarshal(shortNoticeBody, &shortNoticeResult))
	require.Empty(t, shortNoticeResult.Scheduled, "a 1-day-out increase must not be scheduled")
	require.Len(t, shortNoticeResult.Skipped, 1)
	require.Contains(t, shortNoticeResult.Skipped[0].Reason, "notice window")

	effectiveAt := time.Now().UTC().Add(31 * 24 * time.Hour)
	bulkStatus, bulkBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/reprice-all-prior-versions", token, map[string]any{
		"price_key": priceKey, "effective_at": effectiveAt,
	})
	require.Equal(t, http.StatusCreated, bulkStatus, string(bulkBody))
	var batchResult subscriptions.RepriceBatchResult
	require.NoError(t, json.Unmarshal(bulkBody, &batchResult))
	require.Equal(t, 1, batchResult.Matched)
	require.Len(t, batchResult.Scheduled, 1)
	require.Empty(t, batchResult.Skipped)
	require.Equal(t, v2.ID, batchResult.ToPriceID)

	// The price page's pending-migration lookup: find the batch by key
	// without already knowing its id. Two batches exist now — the short-notice
	// attempt above (fully skipped) and this one (fully scheduled) — most
	// recent first.
	batchesStatus, batchesBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/reprices/batches?price_key="+priceKey, token, nil)
	require.Equal(t, http.StatusOK, batchesStatus, string(batchesBody))
	var batches struct {
		Items []*models.RepriceBatch `json:"items"`
	}
	require.NoError(t, json.Unmarshal(batchesBody, &batches))
	require.Len(t, batches.Items, 2)
	require.Equal(t, batchResult.BatchID, batches.Items[0].ID, "most recent (the successful migrate) first")
	require.Equal(t, 1, batches.Items[0].SubscriptionsScheduled)
	require.Equal(t, 0, batches.Items[1].SubscriptionsScheduled, "the short-notice attempt scheduled nothing")

	// Inspect the scheduled row for this batch.
	repricesStatus, repricesBody := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/reprices?reprice_batch_id="+batchResult.BatchID.String(), token, nil)
	require.Equal(t, http.StatusOK, repricesStatus, string(repricesBody))
	var reprices struct {
		Items []*models.SubscriptionReprice `json:"items"`
	}
	require.NoError(t, json.Unmarshal(repricesBody, &reprices))
	require.Len(t, reprices.Items, 1)
	require.Equal(t, models.RepriceStatusScheduled, reprices.Items[0].Status)

	// Cancel before effective: allowed, flips to canceled.
	cancelStatus, cancelBody := requestJSON(t, http.MethodPost,
		surface.BaseURL+"/v1/merchant/reprices/"+reprices.Items[0].ID.String()+"/cancel", token, nil)
	require.Equal(t, http.StatusOK, cancelStatus, string(cancelBody))

	// A second cancel attempt on the same (now-canceled) row is refused.
	recancelStatus, recancelBody := requestJSON(t, http.MethodPost,
		surface.BaseURL+"/v1/merchant/reprices/"+reprices.Items[0].ID.String()+"/cancel", token, nil)
	require.Equal(t, http.StatusConflict, recancelStatus, string(recancelBody))

	// The subscriber is provably untouched throughout.
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT price_id FROM openrails.subscriptions WHERE product_id = $1 AND status = 'active' LIMIT 1`, v1.ProductID).Scan(&pinnedPriceID))
	require.Equal(t, v1.ID, pinnedPriceID, "canceled-before-effective reprice never touches the subscription")
}

// TestStandaloneMerchantRepriceWizardDecreaseHTTP drives a DECREASE: the
// wizard's default is migrate-NOW (no notice-window gate applies to
// decreases), so the bulk call is issued immediately with effective_at=now.
func TestStandaloneMerchantRepriceWizardDecreaseHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"reprice-wizard-dec-"+uuid.NewString(),
		[]string{
			controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate,
			controlplane.PermMerchantSubscriptionsRead, controlplane.PermMerchantSubscriptionsUpdate,
		},
	)

	productKey := "wizard-dec-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	priceKey := productKey + "-monthly"
	publish := func(amount int64) {
		t.Helper()
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": catalog.Manifest{
				Version: catalog.SupportedVersion,
				Products: []catalog.Product{{
					Key:         productKey,
					DisplayName: "Wizard Decrease Product",
					Prices: []catalog.Price{{
						UnitAmount: amount, Currency: "USD", Duration: "30d", AutoRenew: true,
					}},
				}},
			},
			"insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))
	}
	getByKey := func() billingservice.CatalogPrice {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+priceKey, token, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var p billingservice.CatalogPrice
		require.NoError(t, json.Unmarshal(body, &p))
		return p
	}

	publish(1000000) // $10/mo
	v1 := getByKey()
	seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID)

	publish(800000) // -> $8/mo
	v2 := getByKey()
	require.NotEqual(t, v1.ID, v2.ID)

	// Decrease default: migrate everyone now (effective_at = now, no notice
	// window applies to decreases per #773's design).
	bulkStatus, bulkBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/reprice-all-prior-versions", token, map[string]any{
		"price_key": priceKey, "effective_at": time.Now().UTC(),
	})
	require.Equal(t, http.StatusCreated, bulkStatus, string(bulkBody))
	var batchResult subscriptions.RepriceBatchResult
	require.NoError(t, json.Unmarshal(bulkBody, &batchResult))
	require.Equal(t, 1, batchResult.Matched)
	require.Len(t, batchResult.Scheduled, 1)
	require.Equal(t, v2.ID, batchResult.ToPriceID)

	// Scheduled, not yet applied (v1's only effective moment is the next
	// renewal on/after effective_at — never instant, even for a decrease).
	var status models.RepriceStatus
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT status FROM openrails.subscription_reprices WHERE reprice_batch_id = $1`, batchResult.BatchID).Scan(&status))
	require.Equal(t, models.RepriceStatusScheduled, status)
}
