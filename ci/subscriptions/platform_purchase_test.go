//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestPlatformNewCardPurchase(t *testing.T) {
	for _, scenario := range []string{"approved replay", "declined", "foreign application", "foreign tenant", "conversion retry", "conversion response lost", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w := newVaultWorld(t, withPlatformVault)
			client := w.client[remote]
			_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
			require.NoError(t, err)
			_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyPlatform))
			require.NoError(t, err)
			mid := w.client[embedded].MerchantID().UUID().String()
			application := uuid.NewString()
			w.vault.Put(t, "openrails/platform_vault/"+platformTenant, map[string]any{
				"api_key": "platform-private-test", "collection_applications": map[string]any{
					mid: map[string]any{"id": application, "public_api_key": "platform-public-test"},
				},
			})
			bt, endpoint := newBTFake(t)
			bt.tenant, bt.application = platformTenant, application
			bt.decline, bt.failConversion = scenario == "declined", scenario == "conversion retry"
			bt.loseConversionResponse = scenario == "conversion response lost"
			if scenario == "foreign application" {
				bt.application = uuid.NewString()
			}
			if scenario == "foreign tenant" {
				bt.tenant = "another-tenant"
			}
			custody := engine.Graph(w.rt).Runtime.PurchaseService.CustodianSaleService
			custody.BTBaseURLOverride = endpoint
			custody.GatewayDirectPostURLOverride = endpoint + "/nmi"
			keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
			bt.gateway = func(form url.Values, _ string) string {
				return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
					InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator")}).TransactionID
			}
			c := w.newCustomer()
			status, raw := w.pageAt(w.server.URL, http.MethodGet, "/v1/me/config", c.token, "", nil)
			require.Equal(t, http.StatusOK, status)
			require.Contains(t, string(raw), "platform-public-test")
			require.NotContains(t, string(raw), "platform-private-test")
			const number = "4111111111111111"
			o := order{price: w.permanent("platform:purchase").ID, rail: "nmi", token: bt.collect(number)}
			s, err := c.sell(remote, o)
			require.NoError(t, err)
			if scenario == "concurrent" {
				var group sync.WaitGroup
				errors := make(chan error, 2)
				for range 2 {
					group.Go(func() { _, err := s.payAt(t.Context(), w.server.URL, c, o); errors <- err })
				}
				group.Wait()
				close(errors)
				for err := range errors {
					if err != nil {
						var status *billing.StatusError
						require.ErrorAs(t, err, &status)
						require.Equal(t, http.StatusConflict, status.Status)
					}
				}
			}
			paid, err := s.buy(c, o)
			countMethods := func() int {
				var count int
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_methods WHERE customer_id = $1`), c.id).Scan(&count))
				return count
			}
			if scenario == "foreign application" || scenario == "foreign tenant" {
				require.Error(t, err)
				require.Empty(t, bt.salesOf(number))
				require.Zero(t, countMethods())
				return
			}
			require.NoError(t, err)
			require.Len(t, bt.salesOf(number), 1)
			if scenario == "declined" {
				require.Equal(t, "failed", paid.Status)
				require.Zero(t, countMethods())
				bt.mu.Lock()
				require.Empty(t, bt.tokens)
				bt.mu.Unlock()
				replay, err := s.buy(c, o)
				require.NoError(t, err)
				require.Equal(t, "failed", replay.Status)
				require.Len(t, bt.salesOf(number), 1)
				return
			}
			if scenario == "conversion retry" || scenario == "conversion response lost" {
				require.Equal(t, "processing", paid.Status)
				require.Zero(t, countMethods())
				bt.mu.Lock()
				bt.failConversion = false
				bt.mu.Unlock()
				w.until(func() bool { return countMethods() == 1 }, "retained charge finishes card conversion without another sale")
			}
			replay, err := s.buy(c, o)
			require.NoError(t, err)
			// HTTP idempotency replays its original answer; GET reads recovery.
			require.Equal(t, paid.Status, replay.Status)
			require.Equal(t, "succeeded", s.read(c).Status)
			require.Len(t, bt.salesOf(number), 1)
			require.Equal(t, 1, countMethods())
			var custodyKind, containerToken, custodian, chargeVia string
			var wallet *string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT custodian, rail_method_ref, custodian_id::text, charge_via, wallet_card_id::text FROM billing.payment_methods WHERE customer_id = $1`), c.id).
				Scan(&custodyKind, &containerToken, &custodian, &chargeVia, &wallet))
			require.Equal(t, "platform", custodyKind)
			require.Equal(t, "pan_proxy", chargeVia)
			require.Equal(t, w.platformIdentities()[0].id, custodian)
			require.Nil(t, wallet)
			bt.mu.Lock()
			require.Equal(t, []string{"/merchants/" + mid + "/"}, bt.containers[containerToken])
			require.Len(t, bt.tokens, 1)
			bt.mu.Unlock()
		})
	}
}

// Both merchants share the platform tenant and private vault key. An intent
// collected by the first merchant cannot be presented on the second's order.
func TestPlatformCrossMerchantCollection(t *testing.T) {
	bt, endpoint := newBTFake(t)
	bt.tenant = platformTenant
	first, second := newVaultWorld(t, withPlatformVault), newVaultWorld(t, withPlatformVault)
	applications := map[string]any{}
	for _, w := range []*world{first, second} {
		mid := w.client[embedded].MerchantID().UUID().String()
		applications[mid] = map[string]any{"id": uuid.NewString(), "public_api_key": "public-" + mid}
	}
	for _, w := range []*world{first, second} {
		client := w.client[remote]
		_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
		require.NoError(t, err)
		_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyPlatform))
		require.NoError(t, err)
		w.vault.Put(t, "openrails/platform_vault/"+platformTenant, map[string]any{"api_key": "shared-private-test", "collection_applications": applications})
		custody := engine.Graph(w.rt).Runtime.PurchaseService.CustodianSaleService
		custody.BTBaseURLOverride, custody.GatewayDirectPostURLOverride = endpoint, endpoint+"/nmi"
	}
	firstID := first.client[embedded].MerchantID().UUID().String()
	bt.application = applications[firstID].(map[string]any)["id"].(string)
	const number = "4111111111111111"
	token := bt.collect(number)
	buyer := second.newCustomer()
	_, err := buyer.buyOrder(remote, order{price: second.permanent("platform:foreign").ID, rail: "nmi", token: token})
	require.Equal(t, "invalid_param", codeOf(err))
	require.Empty(t, bt.salesOf(number))
	var count int
	require.NoError(t, second.pool.QueryRow(t.Context(), second.q(`SELECT count(*) FROM billing.payment_methods WHERE customer_id=$1`), buyer.id).Scan(&count))
	require.Zero(t, count)
	keyed := first.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, _ string) string {
		return first.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator")}).TransactionID
	}
	first.newCustomer().mustPurchase(remote, order{price: first.permanent("platform:own").ID, rail: "nmi", token: token})
	require.Len(t, bt.salesOf(number), 1)
	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.containers, 1)
	for _, containers := range bt.containers {
		require.Equal(t, []string{"/merchants/" + firstID + "/"}, containers)
	}
}
