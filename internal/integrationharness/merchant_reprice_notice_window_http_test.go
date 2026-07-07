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
	"github.com/open-rails/openrails/pkg/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// #781: server-side notice-window enforcement, end to end over real HTTP —
// the primitive #773 shipped had NO server-side minimum-notice check (only a
// client-side 30-day console gate); this is the replacement for #777's
// gap-proving test (a 1-day-out increase used to be accepted without
// complaint). Covers the full matrix the tracker calls out: typed refusal,
// decrease exemption, the explicit acknowledge override with its audit trail,
// and a merchant-configured window actually being honored.

type repriceErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// noticeWindowFixture publishes one product with a current price (v1), an
// active subscriber pinned to it, and gives the caller a `bump` helper to
// create a successor version (v2) under the same key — everything the
// single-subscription reprice endpoint's notice-window matrix needs.
type noticeWindowFixture struct {
	t                    *testing.T
	ctx                  context.Context
	h                    *Harness
	surface              *Surface
	token                string
	productKey, priceKey string
	v1                   billingservice.CatalogPrice
	subID                uuid.UUID
}

func newNoticeWindowFixture(t *testing.T) *noticeWindowFixture {
	t.Helper()
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"reprice-notice-window-"+uuid.NewString(),
		[]string{
			controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate,
			controlplane.PermMerchantSubscriptionsRead, controlplane.PermMerchantSubscriptionsUpdate,
			controlplane.PermMerchantSettingsRead, controlplane.PermMerchantSettingsUpdate,
		},
	)

	f := &noticeWindowFixture{t: t, ctx: ctx, h: h, surface: surface, token: token}
	f.productKey = "notice-window-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.priceKey = f.productKey + "-monthly"

	f.publish(1000000) // v1 at $10/mo
	f.v1 = f.getByKey()
	f.subID = seedRepriceSubscription(t, ctx, h, f.v1.ProductID, f.v1.ID)
	return f
}

func (f *noticeWindowFixture) putSettings(body map[string]any) {
	f.t.Helper()
	status, respBody := requestJSON(f.t, http.MethodPut, f.surface.BaseURL+"/v1/merchant/settings", f.token, body)
	require.Equal(f.t, http.StatusOK, status, string(respBody))
}

// setNoticeWindowDays exercises the real #781 seam the wizard reads from:
// PUT/GET /v1/merchant/settings.
func (f *noticeWindowFixture) setNoticeWindowDays(days int) {
	f.t.Helper()
	f.putSettings(map[string]any{"reprice_notice_window_days": days})

	status, body := requestJSON(f.t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/settings", f.token, nil)
	require.Equal(f.t, http.StatusOK, status, string(body))
	var settings struct {
		RepriceNoticeWindowDays int `json:"reprice_notice_window_days"`
	}
	require.NoError(f.t, json.Unmarshal(body, &settings))
	require.Equal(f.t, days, settings.RepriceNoticeWindowDays, "the read seam must reflect what was just written")
}

func (f *noticeWindowFixture) publish(amount int64) {
	f.t.Helper()
	status, body := requestJSON(f.t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/catalog/publish", f.token, map[string]any{
		"catalog": catalog.Manifest{
			Version: catalog.SupportedVersion,
			Products: []catalog.Product{{
				Key:         f.productKey,
				DisplayName: "Notice Window Product",
				Prices: []catalog.Price{{
					UnitAmount: amount, Currency: "usd", Duration: "30d", AutoRenew: true,
				}},
			}},
		},
		"insert": true, "overwrite": true,
	})
	require.Equal(f.t, http.StatusOK, status, string(body))
}

func (f *noticeWindowFixture) getByKey() billingservice.CatalogPrice {
	f.t.Helper()
	status, body := requestJSON(f.t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+f.priceKey, f.token, nil)
	require.Equal(f.t, http.StatusOK, status, string(body))
	var p billingservice.CatalogPrice
	require.NoError(f.t, json.Unmarshal(body, &p))
	return p
}

// bump publishes a successor version under the same key (an INCREASE over
// v1) and returns it — the reprice target for the notice-window cases below.
func (f *noticeWindowFixture) bump(amount int64) billingservice.CatalogPrice {
	f.t.Helper()
	f.publish(amount)
	return f.getByKey()
}

func (f *noticeWindowFixture) reprice(toPrice string, effectiveAt time.Time, acknowledge bool) (int, []byte) {
	f.t.Helper()
	return requestJSON(f.t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/subscriptions/"+f.subID.String()+"/reprice", f.token, map[string]any{
		"to_price": toPrice, "effective_at": effectiveAt, "acknowledge_short_notice": acknowledge,
	})
}

// TestStandaloneMerchantRepriceNoticeWindowHTTP_IncreaseInsideWindowRefused:
// the default (unconfigured) window is 30 days — a 1-day-out INCREASE is
// refused with a typed, machine-readable code.
func TestStandaloneMerchantRepriceNoticeWindowHTTP_IncreaseInsideWindowRefused(t *testing.T) {
	f := newNoticeWindowFixture(t)
	f.setNoticeWindowDays(30)
	v2 := f.bump(1200000) // $12/mo, an increase over v1's $10/mo

	status, body := f.reprice(v2.Key, time.Now().UTC().Add(24*time.Hour), false)
	require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
	var envelope repriceErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, "reprice_notice_window_violation", envelope.Error.Code)

	// Fail-closed: nothing was scheduled.
	listStatus, listBody := requestJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/reprices?subscription_id="+f.subID.String(), f.token, nil)
	require.Equal(t, http.StatusOK, listStatus, string(listBody))
	var list struct {
		Items []*models.SubscriptionReprice `json:"items"`
	}
	require.NoError(t, json.Unmarshal(listBody, &list))
	require.Empty(t, list.Items)
}

// TestStandaloneMerchantRepriceNoticeWindowHTTP_DecreaseExempt: a DECREASE
// effective just 1 day out is accepted outright — no notice window applies.
func TestStandaloneMerchantRepriceNoticeWindowHTTP_DecreaseExempt(t *testing.T) {
	f := newNoticeWindowFixture(t)
	f.setNoticeWindowDays(30)
	v2 := f.bump(800000) // $8/mo, a decrease from v1's $10/mo

	status, body := f.reprice(v2.Key, time.Now().UTC().Add(24*time.Hour), false)
	require.Equal(t, http.StatusCreated, status, string(body))
	var rr models.SubscriptionReprice
	require.NoError(t, json.Unmarshal(body, &rr))
	require.Equal(t, models.RepriceStatusScheduled, rr.Status)
	require.False(t, rr.AcknowledgedShortNotice, "a decrease never needs the override")
}

// TestStandaloneMerchantRepriceNoticeWindowHTTP_AcknowledgeOverride_AuditEvidence:
// the explicit acknowledge_short_notice escape hatch schedules the increase
// anyway, and the override is durably recorded — never silent.
func TestStandaloneMerchantRepriceNoticeWindowHTTP_AcknowledgeOverride_AuditEvidence(t *testing.T) {
	f := newNoticeWindowFixture(t)
	f.setNoticeWindowDays(30)
	v2 := f.bump(1200000)

	status, body := f.reprice(v2.Key, time.Now().UTC().Add(24*time.Hour), true)
	require.Equal(t, http.StatusCreated, status, string(body))
	var rr models.SubscriptionReprice
	require.NoError(t, json.Unmarshal(body, &rr))
	require.Equal(t, models.RepriceStatusScheduled, rr.Status)
	require.True(t, rr.AcknowledgedShortNotice, "the override must be visible on the creation response")

	// Independently re-fetch — durable audit evidence, not just an echo.
	getStatus, getBody := requestJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/reprices/"+rr.ID.String(), f.token, nil)
	require.Equal(t, http.StatusOK, getStatus, string(getBody))
	var fetched models.SubscriptionReprice
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	require.True(t, fetched.AcknowledgedShortNotice, "audit evidence persisted on the row, independently re-readable")
}

// TestStandaloneMerchantRepriceNoticeWindowHTTP_MerchantConfiguredWindowRespected:
// a merchant that configures a SHORTER window (via the same settings seam the
// wizard reads from) gets that shorter window enforced — no override needed
// once the request satisfies it.
func TestStandaloneMerchantRepriceNoticeWindowHTTP_MerchantConfiguredWindowRespected(t *testing.T) {
	f := newNoticeWindowFixture(t)
	f.setNoticeWindowDays(2)
	v2 := f.bump(1200000)

	// 3 days out would violate the DEFAULT (30d) window but satisfies this
	// merchant's configured 2-day window — no acknowledge needed.
	status, body := f.reprice(v2.Key, time.Now().UTC().Add(3*24*time.Hour), false)
	require.Equal(t, http.StatusCreated, status, string(body))
	var rr models.SubscriptionReprice
	require.NoError(t, json.Unmarshal(body, &rr))
	require.False(t, rr.AcknowledgedShortNotice)

	// Tightening the window back up refuses the same effective_at for a
	// second subscriber — proves the check re-reads the LIVE configured
	// value on every call, not a value cached at fixture setup.
	f.setNoticeWindowDays(10)
	sub2 := seedRepriceSubscription(t, f.ctx, f.h, f.v1.ProductID, f.v1.ID)
	refuseStatus, refuseBody := requestJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/subscriptions/"+sub2.String()+"/reprice", f.token, map[string]any{
		"to_price": v2.Key, "effective_at": time.Now().UTC().Add(3 * 24 * time.Hour), "acknowledge_short_notice": false,
	})
	require.Equal(t, http.StatusUnprocessableEntity, refuseStatus, string(refuseBody))
}
