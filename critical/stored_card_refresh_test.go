//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

const storedCardRefresh = "openrails.stored_card_refresh"

// vaultCards is a merchant whose new cards a Basis Theory vault holds (its own
// or the platform's), charged through the NMI gateway by number.
type vaultCards struct {
	*authorityWorld
	vendor   *merchantBT
	journal  *nmiJournal
	psp      billing.PSPID
	platform bool
	vault    *vaulttest.Vault
}

func newVaultCards(t *testing.T, platform, armed bool) *vaultCards {
	t.Helper()
	vendor := newMerchantBT(t)
	t.Setenv(vaulttest.AddrEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"))
	t.Setenv(vaulttest.TokenEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN"))
	vault := vaulttest.New(t)
	a, journal := newNMIWorld(t, func(w *world) {
		w.cfg.Vault = vault.Config()
		w.deps.Vault = vault.Client()
		if platform {
			w.cfg.CardVault = &openrails.CardVaultConfig{Kind: "basis_theory", TenantID: merchantBTTenant, KVMount: vault.Config().KVMount}
		}
		vendor.gateway = w.deps.NMITransport
		target, err := url.Parse(vendor.server.URL)
		require.NoError(t, err)
		wire := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(wire.CloseIdleConnections)
		w.deps.BasisTheoryTransport = custodianProbeWire{loopback: loopbackWire{target: target, wire: wire}, wire: wire}
		for name, merchant := range w.cfg.Merchants {
			merchant.DisplayName = ""
			merchant.PSPs = nil
			merchant.Custodians = nil
			merchant.CardVault = ""
			w.cfg.Merchants[name] = merchant
		}
	})
	v := &vaultCards{authorityWorld: a, vendor: vendor, journal: journal, platform: platform, vault: vault}
	custody, cardVault := "owned", "nmi"
	if platform {
		custody, cardVault = "", "platform"
		v.armPlatform(t, armed)
	} else {
		_, err := a.alphaRemote.PutCustodian(t.Context(), "owned", billing.PutCustodianParams{ExpectedRevision: 0, Kind: "basis_theory", Environment: "test", AccountID: merchantBTTenant,
			Settings: map[string]any{"public_api_key": "public-merchant", "account_updater": armed}, Credentials: map[string]string{"api_key": "private-merchant"}})
		require.NoError(t, err)
	}
	_, err := a.alphaRemote.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "nmi", Rail: billing.RailNMI, Environment: "test", Custodian: custody, PSPAccount: billing.PSPAccount{GatewayID: "critical-gateway"}, TokenizationKey: "critical-collect-key", Credentials: map[string]string{"security_key": "critical-nmi-key", "webhook_signing_secret": "critical-nmi-webhook"}})
	require.NoError(t, err)
	_, err = a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{CardVault: cardVault}})
	require.NoError(t, err)
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	v.psp = psps.Items[0].ID
	return v
}

// armPlatform writes the operator's platform vault document.
func (v *vaultCards) armPlatform(t *testing.T, armed bool) {
	t.Helper()
	_, err := v.deps.Vault.Logical().WriteWithContext(t.Context(), v.cfg.Vault.KVMount+"/data/openrails/platform_vault/"+merchantBTTenant, map[string]any{"data": map[string]any{
		"api_key": "private-merchant", "account_updater": armed,
		"collection_applications": map[string]any{v.alpha.MerchantID().UUID().String(): map[string]string{"id": merchantBTApplication, "public_api_key": "public-merchant"}}}})
	require.NoError(t, err)
}

// saveCard saves a card with its own number for the bearer's customer.
func (v *vaultCards) saveCard(t *testing.T, bearer, number, brand, last4 string) billing.PaymentMethod {
	t.Helper()
	v.journal.core.AddNumber(number, nmimock.Card{Brand: brand, Last4: last4})
	token := v.vendor.collectCard(merchantBTTenant, number, basistheory.CardDetails{Brand: brand, Last4: last4, ExpirationMonth: 12, ExpirationYear: 2035})
	status, raw := v.request(t, http.MethodPost, "/v1/me/payment-methods", bearer, billing.CreatePaymentMethodParams{PSPID: v.psp, Token: token})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var method billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &method))
	require.Equal(t, billing.PaymentMethodActive, method.Status)
	return method
}

// subscribe buys a membership renewing every hours with a saved card.
func (v *vaultCards) subscribe(t *testing.T, bearer, key string, hours int, amount int64, method billing.PaymentMethod) billing.Subscription {
	t.Helper()
	product, err := v.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key + ":member"}})
	require.NoError(t, err)
	price, err := v.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: amount, Currency: "USD", BillingIntervalHours: ptr(hours)})
	require.NoError(t, err)
	buy := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(amount), Payment: &billing.OrderPaymentParams{PaymentMethodID: &method.ID}}
	status, raw := v.request(t, http.MethodPost, "/v1/me/orders", bearer, buy, map[string]string{"Idempotency-Key": "buy-" + key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status, string(raw))
	require.NotNil(t, order.Lines[0].SubscriptionID)
	sub, err := v.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, "engine", string(sub.CollectionPolicy))
	return *sub
}

func (v *vaultCards) card(t *testing.T, method billing.PaymentMethod) billing.PaymentMethod {
	t.Helper()
	page, err := v.alpha.ListPaymentMethods(t.Context(), method.CustomerID, billing.PaymentMethodListParams{})
	require.NoError(t, err)
	for _, item := range page.Items {
		if item.ID == method.ID {
			return item
		}
	}
	t.Fatalf("payment method %s is gone", method.ID)
	return billing.PaymentMethod{}
}

func noticeCount(t *testing.T, a *authorityWorld, kind string) int {
	t.Helper()
	facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{kind}, Limit: 100})
	require.NoError(t, err)
	return len(facts.Events)
}

func refreshJobs(t *testing.T, fleet *river.Client[pgx.Tx]) int {
	t.Helper()
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(storedCardRefresh).First(100))
	require.NoError(t, err)
	return len(jobs.Jobs)
}

func TestStoredCardRefresh(t *testing.T) {
	for _, profile := range []string{"merchant-owned-vault", "hosted-isolated-vault"} {
		t.Run("OR-J12-S08/"+profile, func(t *testing.T) {
			v := newVaultCards(t, profile == "hosted-isolated-vault", true)
			method := v.saveCard(t, v.aliceToken, "4111111111111111", "visa", "1111")
			sub := v.subscribe(t, v.aliceToken, "vault-renewal", 24, 23_000_000, method)
			v.journal.money(t, 1, 2300)
			before := v.card(t, method)
			require.NotNil(t, before.Card.Last4)
			require.Equal(t, "1111", *before.Card.Last4)
			// Checked ahead of its renewal: nothing yet.
			fleet := startFleet(t, v.world)
			checkAhead(t, v, fleet, 1)

			// The issuer reissued the card: the old number declines as
			// expired and the vault's updater holds the new one.
			v.journal.core.SetDecline("1111", "223")
			v.vendor.mu.Lock()
			var oldToken string
			for id, token := range v.vendor.tokens {
				if token.Card != nil && token.Card.Last4 == "1111" {
					oldToken = id
				}
			}
			v.vendor.mu.Unlock()
			require.NotEmpty(t, oldToken)
			v.vendor.reissue(oldToken, btReissue{code: basistheory.AUUpdatedPAN, number: "4242424242424242", card: basistheory.CardDetails{Brand: "visa", Last4: "4242", ExpirationMonth: 11, ExpirationYear: 2031}})

			due := *sub.CurrentPeriodEndsAt
			v.clock.Advance(due.Sub(v.clock.Now()))
			runRegistered(t, fleet, "openrails.dunning", 2)
			require.Eventually(t, func() bool {
				card := v.card(t, method)
				return card.Card.Last4 != nil && *card.Card.Last4 == "4242"
			}, 20*time.Second, 20*time.Millisecond, "the declined card is refreshed at its vault")
			refreshed := v.card(t, method)
			require.Equal(t, billing.PaymentMethodActive, refreshed.Status)
			require.EqualValues(t, 11, *refreshed.Card.ExpMonth)
			require.EqualValues(t, 2031, *refreshed.Card.ExpYear)
			require.Equal(t, []string{oldToken, oldToken}, v.vendor.updaterAsked(), "asked again at the decline")
			v.journal.money(t, 1, 2300)

			// The renewal waiting on the card retries on the new one.
			runRegistered(t, fleet, "openrails.dunning", 2)
			require.Eventually(t, func() bool {
				current, err := v.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.After(due)
			}, 20*time.Second, 20*time.Millisecond, "the renewal is collected on the reissued card")
			current, err := v.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			require.Equal(t, "active", string(current.Status))
			v.journal.money(t, 2, 4600)
			last := v.journal.core.LastSale()
			require.NotNil(t, last)
			require.Equal(t, "4242", last.Card.Last4)
			require.Equal(t, 1, noticeCount(t, v.authorityWorld, "payment_method_failed"))
			require.Zero(t, noticeCount(t, v.authorityWorld, "payment_method_update_required"), "a refreshed card never asks the customer")
			require.Len(t, v.vendor.updaterAsked(), 2)
		})
	}

	t.Run("OR-J12-S09/unchanged-vault-card-asks-the-customer", func(t *testing.T) {
		v := newVaultCards(t, false, true)
		method := v.saveCard(t, v.aliceToken, "4111111111111111", "visa", "1111")
		sub := v.subscribe(t, v.aliceToken, "vault-renewal", 24, 23_000_000, method)
		fleet := startFleet(t, v.world)
		checkAhead(t, v, fleet, 1)
		require.Zero(t, noticeCount(t, v.authorityWorld, "payment_method_update_required"), "a check ahead of renewal asks nobody")
		v.journal.core.SetDecline("1111", "223")
		due := *sub.CurrentPeriodEndsAt
		v.clock.Advance(due.Sub(v.clock.Now()))
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, v.authorityWorld, sub.ID, "awaiting_method")
		require.Eventually(t, func() bool { return noticeCount(t, v.authorityWorld, "payment_method_update_required") == 1 }, 20*time.Second, 20*time.Millisecond,
			"no new card at the vault: the customer is asked")
		require.Equal(t, 1, noticeCount(t, v.authorityWorld, "payment_method_failed"))
		require.Len(t, v.vendor.updaterAsked(), 2, "asked again at the decline")
		require.Equal(t, "1111", *v.card(t, method).Card.Last4)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, v.authorityWorld, sub.ID, "awaiting_method")
		v.journal.money(t, 1, 2300)
		require.Len(t, v.vendor.updaterAsked(), 2)
		require.Equal(t, 1, noticeCount(t, v.authorityWorld, "payment_method_update_required"))
	})

	t.Run("OR-J12-S10/ahead-of-renewal-pass-picks-due-cards-of-armed-vaults", func(t *testing.T) {
		v := newVaultCards(t, true, false)
		renewing := v.saveCard(t, v.aliceToken, "4111111111111111", "visa", "1111")
		v.subscribe(t, v.aliceToken, "daily", 24, 23_000_000, renewing)
		monthly := v.saveCard(t, v.aliceToken, "5555555555554444", "mastercard", "4444")
		v.subscribe(t, v.aliceToken, "monthly", 30*24, 31_000_000, monthly)
		v.saveCard(t, v.aliceToken, "4000056655665556", "visa", "5556")
		waiting := v.saveCard(t, v.bobToken, "4012888888881881", "visa", "1881")
		declined := v.subscribe(t, v.bobToken, "bob-daily", 24, 17_000_000, waiting)
		v.journal.money(t, 3, 7100)

		// An unarmed vault's decline asks the customer at once.
		v.journal.core.SetDecline("1881", "223")
		due := *declined.CurrentPeriodEndsAt
		v.clock.Advance(due.Sub(v.clock.Now()))
		fleet := startFleet(t, v.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, v.authorityWorld, declined.ID, "awaiting_method")
		require.Eventually(t, func() bool { return noticeCount(t, v.authorityWorld, "payment_method_update_required") == 1 }, 20*time.Second, 20*time.Millisecond)
		require.Zero(t, noticeCount(t, v.authorityWorld, "payment_method_failed"))
		v.journal.money(t, 4, 9400)

		runRegistered(t, fleet, "openrails.stored_card_refresh_due", 1)
		require.Zero(t, refreshJobs(t, fleet), "an unarmed vault's cards are never refreshed")
		require.Empty(t, v.vendor.updaterAsked())

		v.armPlatform(t, true)
		runRegistered(t, fleet, "openrails.stored_card_refresh_due", 1)
		require.Eventually(t, func() bool { return len(v.vendor.updaterAsked()) == 2 }, 20*time.Second, 20*time.Millisecond)
		asked := v.vendor.updaterAsked()
		v.vendor.mu.Lock()
		var last4s []string
		for _, id := range asked {
			last4s = append(last4s, v.vendor.tokens[id].Card.Last4)
		}
		v.vendor.mu.Unlock()
		slices.Sort(last4s)
		require.Equal(t, []string{"1111", "1881"}, last4s, "the renewing and the awaiting-method cards; not the unused card or the renewal beyond the lookahead")
		require.Eventually(t, func() bool { return refreshJobsDone(t, fleet) == 2 }, 20*time.Second, 20*time.Millisecond)

		runRegistered(t, fleet, "openrails.stored_card_refresh_due", 1)
		require.Equal(t, 2, refreshJobs(t, fleet), "a card checked inside its lookahead is not checked again")
		require.Len(t, v.vendor.updaterAsked(), 2)
		require.Equal(t, 1, noticeCount(t, v.authorityWorld, "payment_method_update_required"), "a refresh ahead of renewal asks nobody")
	})

	t.Run("OR-J12-S11/psp-held-card-declined-as-expired-asks-the-customer", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		p.core.SetDecline("4242", "expired_card")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		require.Eventually(t, func() bool { return noticeCount(t, a, "payment_method_update_required") == 1 }, 20*time.Second, 20*time.Millisecond)
		require.Zero(t, noticeCount(t, a, "payment_method_failed"), "the PSP charged its current card: nothing to re-read")
		require.Zero(t, refreshJobs(t, fleet))
	})
}

// checkAhead runs the ahead-of-renewal pass and waits for its refreshes: the
// vault has then been asked calls times in all.
func checkAhead(t *testing.T, v *vaultCards, fleet *river.Client[pgx.Tx], calls int) {
	t.Helper()
	runRegistered(t, fleet, "openrails.stored_card_refresh_due", 1)
	require.Eventually(t, func() bool {
		return len(v.vendor.updaterAsked()) == calls && refreshJobsDone(t, fleet) == refreshJobs(t, fleet)
	}, 20*time.Second, 20*time.Millisecond)
}

func refreshJobsDone(t *testing.T, fleet *river.Client[pgx.Tx]) int {
	t.Helper()
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(storedCardRefresh).States(rivertype.JobStateCompleted).First(100))
	require.NoError(t, err)
	return len(jobs.Jobs)
}
