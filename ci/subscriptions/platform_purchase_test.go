//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

func TestPlatformNewCardPurchase(t *testing.T) {
	for _, scenario := range []string{"approved replay", "unavailable merchant custodian", "declined", "duplicate refused", "foreign application", "foreign tenant", "conversion retry", "conversion response lost", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12, withPlatformVault)
			w.vault = vaulttest.New(t)
			if scenario == "unavailable merchant custodian" {
				w.custodians = map[string]openrails.CustodianConfig{"bt": {
					Kind: "basis_theory", AccountID: "merchant-tenant",
					Settings: map[string]any{"public_api_key": "merchant-public-test"},
				}}
				w.declare = func(psps map[string]openrails.PSPConfig) {
					psps["nmi"] = openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "e2e-nmi", SecurityKey: "e2e-nmi-key", Custodian: "bt"}.PSPConfig()
				}
			}
			w.start()
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
			if scenario == "duplicate refused" {
				bt.saleResponse = "response=3&response_code=300&responsetext=Duplicate+transaction+REFID%3A123"
			}
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
			preview, err := client.PreviewPSPRouting(t.Context(), billing.PreviewPSPRoutingParams{PriceID: o.price, PSP: "nmi"})
			require.NoError(t, err)
			require.NotNil(t, preview.PSP)
			require.Equal(t, "nmi", *preview.PSP)
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
			if scenario == "declined" || scenario == "duplicate refused" {
				require.Equal(t, "failed", paid.Status)
				if scenario == "duplicate refused" {
					require.NotNil(t, paid.Failure)
					require.Equal(t, "processing_error", paid.Failure.Reason)
				}
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

func TestPlatformVaultUnavailableKeepsExistingPSPCard(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t, withPlatformVault)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	client := w.client[remote]
	_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
	require.NoError(t, err)
	_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyPlatform))
	require.NoError(t, err)
	// No platform credential document: only the customer's existing gateway
	// card can pay. Choosing custody for new cards must not disable it.
	c.mustPurchase(remote, order{price: w.permanent("platform:existing").ID, method: method})
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

func TestPlatformSavedCardOrder(t *testing.T) {
	for _, scenario := range []string{"paid replay", "response lost", "concurrent", "declined", "duplicate refused", "custody switched", "consent revoked", "consent revoked before submission", "closed", "foreign customer", "foreign container", "foreign tenant", "original account unavailable", "recurring order"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12, withPlatformVault)
			w.vault = vaulttest.New(t)
			if scenario == "original account unavailable" {
				routing := []billing.PSPRoutingRule{{Prefer: []string{"nmi", "nmi-again"}}}
				w.settings = billing.MerchantSettings{PSPRouting: &routing}
				w.declare = func(psps map[string]openrails.PSPConfig) {
					psps["nmi-again"] = openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "e2e-nmi-again", SecurityKey: "e2e-nmi-again-key", WebhookSigningSecret: "nmi_again_webhook_e2e"}.PSPConfig()
				}
			}
			w.start()
			client := w.client[remote]
			_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
			require.NoError(t, err)
			_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyPlatform))
			require.NoError(t, err)
			mid := w.client[embedded].MerchantID().UUID().String()
			application := uuid.NewString()
			w.vault.Put(t, "openrails/platform_vault/"+platformTenant, map[string]any{"api_key": "platform-private-test", "collection_applications": map[string]any{
				mid: map[string]any{"id": application, "public_api_key": "platform-public-test"},
			}})
			bt, endpoint := newBTFake(t)
			bt.tenant, bt.application = platformTenant, application
			custody := engine.Graph(w.rt).Runtime.PurchaseService.CustodianSaleService
			custody.BTBaseURLOverride, custody.GatewayDirectPostURLOverride = endpoint, endpoint+"/nmi"
			keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
			bt.gateway = func(form url.Values, _ string) string {
				return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
					InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator")}).TransactionID
			}
			c := w.newCustomer()
			const number = "4111111111111111"
			first, err := c.buyOrder(remote, order{price: w.permanent("platform:first").ID, rail: "nmi", token: bt.collect(number)})
			require.NoError(t, err)
			require.Equal(t, "succeeded", first.Status)
			methods := c.paymentMethods()
			require.Len(t, methods, 1)
			method := methods[0]["id"].(string)
			require.Equal(t, true, methods[0]["reusable"])
			next := order{price: w.permanent("platform:next").ID, method: method}
			sale, err := c.sell(remote, next)
			require.NoError(t, err)
			view := sale.read(c).Order
			require.NotEmpty(t, view.PaymentOptions)
			if scenario == "original account unavailable" {
				require.Len(t, view.PaymentOptions, 2)
			}
			for _, option := range view.PaymentOptions {
				if option.PSPID == w.psp["nmi"] {
					require.Equal(t, []billing.PaymentMethodID{*first.Order.Payment.PaymentMethodID}, option.SavedPaymentMethodIDs)
					require.Contains(t, option.Accepts, "saved_card")
				} else {
					require.Empty(t, option.SavedPaymentMethodIDs)
				}
			}

			if scenario == "paid replay" {
				svc, err := service.New(engine.Graph(w.rt).Runtime)
				require.NoError(t, err)
				ctx := paymentpage.WithPayURL(merchant.WithID(t.Context(), w.client[embedded].MerchantID()), paymentpage.PayURL{CustomerID: c.cid(), OrderID: sale.order.ID, SavedPaymentMethods: false})
				hidden, err := svc.GetOrder(ctx, service.OrderActor{Customer: c.cid()}, sale.order.ID)
				require.NoError(t, err)
				for _, option := range hidden.PaymentOptions {
					require.Empty(t, option.SavedPaymentMethodIDs)
				}
			}

			switch scenario {
			case "original account unavailable":
				_, err = client.UpdatePSP(t.Context(), w.psp["nmi"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
				require.NoError(t, err)
				remaining := sale.read(c).Order.PaymentOptions
				require.NotEmpty(t, remaining)
				for _, option := range remaining {
					require.Equal(t, w.psp["nmi-again"], option.PSPID)
					require.Empty(t, option.SavedPaymentMethodIDs)
				}
			case "recurring order":
				next.price = w.membership("platform:recurring", 10_000_000).ID
				sale, err = c.sell(remote, next)
				require.NoError(t, err)
			case "custody switched":
				_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyMerchant))
				require.NoError(t, err)
			case "consent revoked":
				c.must(http.MethodPatch, "/payment-methods/"+method, "", map[string]any{"reusable": false})
			case "consent revoked before submission":
				bt.mu.Lock()
				bt.beforeTokenRead = func() {
					_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.mandates SET status = 'revoked', end_reason = 'customer_revoked', ended_at = now() WHERE payment_method_id = $1 AND kind = 'card_on_file'`), first.Order.Payment.PaymentMethodID.UUID())
					if err != nil {
						t.Error(err)
					}
				}
				bt.mu.Unlock()
			case "closed":
				_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.payment_methods SET status = 'closed' WHERE id = $1`), first.Order.Payment.PaymentMethodID.UUID())
				require.NoError(t, err)
			case "foreign customer":
				c = w.newCustomer()
				sale, err = c.sell(remote, next)
				require.NoError(t, err)
			case "foreign container":
				bt.mu.Lock()
				for token := range bt.tokens {
					bt.containers[token] = []string{"/merchants/" + uuid.NewString() + "/"}
				}
				bt.mu.Unlock()
			case "foreign tenant":
				bt.mu.Lock()
				bt.tenant = uuid.NewString()
				bt.mu.Unlock()
			case "response lost":
				bt.mu.Lock()
				bt.loseSaleResponse = true
				bt.mu.Unlock()
			case "duplicate refused":
				bt.mu.Lock()
				bt.saleResponse = "response=3&response_code=300&responsetext=Duplicate+transaction+REFID%3A123"
				bt.mu.Unlock()
			case "declined":
				bt.mu.Lock()
				bt.decline = true
				bt.mu.Unlock()
			}
			if scenario == "concurrent" {
				var group sync.WaitGroup
				for range 2 {
					group.Go(func() { _, _ = sale.payAt(t.Context(), w.server.URL, c, next) })
				}
				group.Wait()
			}
			paid, err := sale.buy(c, next)
			switch scenario {
			case "consent revoked", "closed", "foreign customer", "original account unavailable", "recurring order":
				require.Error(t, err)
				for _, option := range sale.read(c).Order.PaymentOptions {
					require.Empty(t, option.SavedPaymentMethodIDs)
				}
				require.Len(t, bt.salesOf(number), 1)
				return
			case "foreign container", "foreign tenant", "consent revoked before submission":
				require.NoError(t, err)
				require.Equal(t, "failed", paid.Status)
				require.Len(t, bt.salesOf(number), 1)
				return
			case "declined", "duplicate refused":
				require.NoError(t, err)
				if scenario == "duplicate refused" {
					require.NotNil(t, paid.Failure)
					require.Equal(t, "processing_error", paid.Failure.Reason)
					require.Len(t, w.nmi.ledger(""), 1, "the refused request creates no second charge")
					var terminal, duplicate bool
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status = 'failed_terminal', (result_evidence->>'duplicate_refused')::boolean FROM billing.provider_intents WHERE payload->>'order_id' = $1 AND intent_type = 'nmi_sale'`), sale.order.ID.UUID().String()).Scan(&terminal, &duplicate))
					require.True(t, terminal)
					require.True(t, duplicate)
				}
				require.Equal(t, "failed", paid.Status)
				_, err = sale.buy(c, next)
				require.NoError(t, err)
				require.Len(t, bt.salesOf(number), 2)
				return
			}
			require.NoError(t, err)
			w.until(func() bool { return sale.read(c).Status == "succeeded" }, "the saved card order settles")
			_, err = sale.buy(c, next)
			require.NoError(t, err)
			sales := bt.salesOf(number)
			require.Len(t, sales, 2, "replay and recovery never submit another payment")
			require.Equal(t, "token", sales[1].Source)
			require.Empty(t, sales[1].CVV)
			require.Equal(t, "customer", sales[1].InitiatedBy)
			require.Equal(t, "used", sales[1].Indicator)
			require.Equal(t, sales[0].TransactionID, sales[1].InitialTxnID)
			require.Equal(t, sales[0].SecurityKey, sales[1].SecurityKey)
			require.Len(t, c.paymentMethods(), 1, "reuse does not create another card")
		})
	}
}
