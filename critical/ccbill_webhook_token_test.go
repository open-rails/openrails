//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/stretchr/testify/require"
)

const (
	ccbillToken         = "critical-ccbill-webhook-token-0123456789"
	ccbillPreviousToken = "critical-ccbill-previous-token-0123456789"
	ccbillWrongToken    = "critical-ccbill-wrong-webhook-token-0123"
)

// ccbillTokenWorld is ccbillWebhookWorld behind the merchant's own proxy
// (loopback), its CCBill PSP declared by declare.
func ccbillTokenWorld(t *testing.T, declare func(w *world, psp *openrails.CCBillPSP)) *authorityWorld {
	t.Helper()
	return ccbillWebhookWorld(t, func(w *world) {
		w.cfg.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
		psp := openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentSandbox, Salt: "critical-ccbill-salt"}
		declare(w, &psp)
		m := cfgMerchant(w.cfg, "alpha")
		m.PSPs = billing.PSPs{"ccbill": psp.PSPConfig()}
		setCfgMerchant(&w.cfg, "alpha", m)
	})
}

// ccbillTokenNotice posts a CCBill webhook for 999999-0000 to its URL with
// token as the last path segment ("" posts to the tokenless URL), from the
// address forwardedFor names ("" the loopback proxy itself).
func ccbillTokenNotice(t *testing.T, a *authorityWorld, rail, token, forwardedFor, eventType string, fields map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"clientAccnum": "999999", "clientSubacc": "0000", "timestamp": a.clock.Now().UTC().Format(time.DateTime)}
	for k, v := range fields {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	path := "/v1/webhooks/" + rail + "/999999-0000"
	if token != "" {
		path += "/" + token
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+path+"?eventType="+eventType, bytes.NewReader(raw))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(out)
}

// ccbillProbe is a delivery of an event type OpenRails takes no action on:
// accepted once the delivery is authenticated.
func ccbillProbe(t *testing.T, a *authorityWorld, token string) (int, string) {
	t.Helper()
	return ccbillTokenNotice(t, a, "ccbill", token, "", "UserReactivationProbe", nil)
}

// webhookAccepted are the PSPs a verified webhook reached.
func webhookAccepted(t *testing.T, w *world) []uuid.UUID {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT psp_id FROM "+pgx.Identifier{w.schema, "webhook_health"}.Sanitize()+" WHERE last_accepted_at IS NOT NULL AND psp_id IS NOT NULL ORDER BY psp_id")
	require.NoError(t, err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	require.NoError(t, err)
	return ids
}

func requireInvalidToken(t *testing.T, status int, body string) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.Contains(t, body, `"authentication_required"`)
	require.Contains(t, body, "Invalid webhook token")
}

func TestCCBillWebhookToken(t *testing.T) {
	t.Run("OR-J28-S17/a-declared-webhook-token-is-required-and-the-source-address-still-is", func(t *testing.T) {
		a := ccbillTokenWorld(t, func(_ *world, psp *openrails.CCBillPSP) { psp.WebhookToken = ccbillToken })
		price := ccbillMemberPrice(t, a.alpha)
		alice := customerOf(a.alice)
		now := a.clock.Now().UTC().Truncate(time.Second)
		due := now.Add(-time.Hour)
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due))
		next := due.Add(720 * time.Hour)
		renewal := map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778801", "billedAmount": "10.00", "billedCurrencyCode": "840",
			"timestamp": now.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly), "cardType": "VISA", "last4": "1111"}

		status, body := ccbillTokenNotice(t, a, "ccbill", "", "", "RenewalSuccess", renewal)
		requireInvalidToken(t, status, body)
		status, body = ccbillTokenNotice(t, a, "ccbill", ccbillWrongToken, "", "RenewalSuccess", renewal)
		requireInvalidToken(t, status, body)
		status, body = ccbillTokenNotice(t, a, "ccbill", ccbillToken, "203.0.113.5", "RenewalSuccess", renewal)
		require.Equal(t, http.StatusForbidden, status, "the token never replaces the source address: %s", body)
		require.True(t, onlySubscription(t, a.alpha, alice).CurrentPeriodEndsAt.Equal(due), "a refused renewal extends nothing")
		require.Empty(t, ccbillPayments(t, a.world, alice)["0912233445566778801"])

		status, body = ccbillTokenNotice(t, a, "ccbill", ccbillToken, "", "RenewalSuccess", renewal)
		require.Equal(t, http.StatusOK, status, body)
		sub := onlySubscription(t, a.alpha, alice)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(time.Date(next.Year(), next.Month(), next.Day(), 23, 59, 59, 0, time.UTC)), "paid through %s", sub.CurrentPeriodEndsAt)
		require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)["0912233445566778801"])
	})
	t.Run("OR-J28-S18/a-rotated-out-webhook-token-verifies-only-inside-its-overlap", func(t *testing.T) {
		a := ccbillTokenWorld(t, func(w *world, psp *openrails.CCBillPSP) {
			psp.WebhookToken, psp.WebhookTokenPrevious = ccbillToken, ccbillPreviousToken
			psp.WebhookOverlapExpiresAt = w.clock.Now().Add(time.Hour)
		})
		status, body := ccbillProbe(t, a, ccbillPreviousToken)
		require.Equal(t, http.StatusOK, status, body)
		status, body = ccbillProbe(t, a, ccbillToken)
		require.Equal(t, http.StatusOK, status, body)
		a.clock.Advance(time.Hour)
		status, body = ccbillProbe(t, a, ccbillPreviousToken)
		requireInvalidToken(t, status, body)
		status, body = ccbillProbe(t, a, ccbillToken)
		require.Equal(t, http.StatusOK, status, body)
	})
	t.Run("OR-J28-S19/without-a-declared-token-the-tokenless-url-works-and-the-token-url-is-refused", func(t *testing.T) {
		a := ccbillTokenWorld(t, func(*world, *openrails.CCBillPSP) {})
		status, body := ccbillProbe(t, a, "")
		require.Equal(t, http.StatusOK, status, body)
		status, body = ccbillProbe(t, a, ccbillToken)
		requireInvalidToken(t, status, body)
	})
	t.Run("OR-J28-S20/require-webhook-token-false-admits-a-tokenless-delivery-never-a-wrong-token", func(t *testing.T) {
		a := ccbillTokenWorld(t, func(_ *world, psp *openrails.CCBillPSP) {
			psp.WebhookToken, psp.RequireWebhookToken = ccbillToken, ptr(false)
		})
		status, body := ccbillProbe(t, a, "")
		require.Equal(t, http.StatusOK, status, body)
		status, body = ccbillProbe(t, a, ccbillToken)
		require.Equal(t, http.StatusOK, status, body)

		price := ccbillMemberPrice(t, a.alpha)
		alice := customerOf(a.alice)
		now := a.clock.Now().UTC().Truncate(time.Second)
		due := now.Add(-time.Hour)
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due))
		status, body = ccbillTokenNotice(t, a, "ccbill", ccbillWrongToken, "", "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider,
			"transactionId": "0912233445566778801", "billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": now.Format(time.DateTime),
			"nextRenewalDate": due.Add(720 * time.Hour).Format(time.DateOnly), "cardType": "VISA", "last4": "1111"})
		requireInvalidToken(t, status, body)
		require.True(t, onlySubscription(t, a.alpha, alice).CurrentPeriodEndsAt.Equal(due), "a wrong token's renewal extends nothing")
		require.Empty(t, ccbillPayments(t, a.world, alice)["0912233445566778801"])
	})
	t.Run("OR-J28-S22/a-tokenless-delivery-is-never-handed-to-a-sibling-psp-without-a-token", func(t *testing.T) {
		a := newAccountClassWorld(t, billing.PSPs{
			"ccbill-live":    openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentLive, Salt: "critical-ccbill-salt", WebhookToken: ccbillToken}.PSPConfig(),
			"ccbill-sandbox": openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentSandbox, Salt: "critical-ccbill-salt"}.PSPConfig(),
		}, nil, func(w *world) { w.cfg.TrustedProxies = []string{"127.0.0.1/32", "::1/128"} })
		ids := pspIDs(t, a.alpha)
		require.Len(t, ids, 2)
		const fromCCBill = "64.38.212.10"
		status, body := ccbillTokenNotice(t, a, "ccbill", "", fromCCBill, "UserReactivationProbe", nil)
		requireInvalidToken(t, status, body)
		require.Empty(t, webhookAccepted(t, a.world), "neither PSP took the tokenless delivery")
		status, body = ccbillTokenNotice(t, a, "ccbill", ccbillToken, fromCCBill, "UserReactivationProbe", nil)
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, []uuid.UUID{ids["ccbill-live"]}, webhookAccepted(t, a.world))
	})
	t.Run("OR-J28-S23/rotating-the-webhook-token-over-the-api-keeps-the-old-one-for-its-overlap", func(t *testing.T) {
		const next = "critical-ccbill-rotated-token-0123456789"
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		a, _ := newPSPWorld(t, func(w *world) {
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillDataLinkURL: dataLink.URL(), CCBillWebhookSources: []string{"127.0.0.1/32"}}
		})
		dataLink.SetClock(a.clock.Now)
		psp, err := a.alphaRemote.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "ccbill", Rail: billing.RailCCBill, Environment: billing.EnvironmentSandbox,
			PSPAccount: billing.PSPAccount{ClientAccnum: "999999", ClientSubacc: "0000"}, Credentials: map[string]string{"salt": "critical-ccbill-salt",
				"datalink_username": "critical-datalink", "datalink_password": "critical-datalink-password", "webhook_token": ccbillToken}})
		require.NoError(t, err)
		_, err = a.alphaRemote.UpdatePSP(t.Context(), psp.ID, billing.UpdatePSPParams{Credentials: map[string]string{"webhook_token_previous": ccbillPreviousToken}})
		status, _ := refusalCode(t, err)
		require.Equal(t, http.StatusBadRequest, status, "the previous token is kept by rotation, never supplied")
		_, err = a.alphaRemote.UpdatePSP(t.Context(), psp.ID, billing.UpdatePSPParams{Credentials: map[string]string{"webhook_token": next}})
		require.NoError(t, err)
		for _, token := range []string{ccbillToken, next} {
			status, body := ccbillProbe(t, a, token)
			require.Equal(t, http.StatusOK, status, body)
		}
		a.clock.Advance(24 * time.Hour)
		status, body := ccbillProbe(t, a, ccbillToken)
		requireInvalidToken(t, status, body)
		status, body = ccbillProbe(t, a, next)
		require.Equal(t, http.StatusOK, status, body)
	})
	t.Run("OR-J28-S21/the-token-url-exists-only-for-ccbill", func(t *testing.T) {
		a := ccbillTokenWorld(t, func(_ *world, psp *openrails.CCBillPSP) { psp.WebhookToken = ccbillToken })
		for _, rail := range []string{"nmi", "stripe", "solana", "basistheory"} {
			status, body := ccbillTokenNotice(t, a, rail, ccbillToken, "", "Probe", nil)
			require.Equal(t, http.StatusNotFound, status, "%s: %s", rail, body)
			require.Contains(t, body, `"route_not_found"`)
		}
	})
}
