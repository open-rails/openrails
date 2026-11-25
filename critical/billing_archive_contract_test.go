//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/stripemock"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/stretchr/testify/require"
)

func archiveTarget(t *testing.T, source *world, configure ...func(*world)) (*world, *openrails.Client) {
	t.Helper()
	configure = append([]func(*world){func(w *world) {
		w.posture = "full"
		w.deps.StripeTransport = source.deps.StripeTransport
	}}, configure...)
	target := newWorld(t, configure...)
	id := source.alpha.MerchantID()
	hostCLI(t, target, "billing", "prepare-target", "--merchant", id.String(), "--slug", "restored-source", "--unbound-merchants")
	client, err := target.engine.Merchant(id)
	require.NoError(t, err)
	return target, client
}
func archiveBytes(t *testing.T, client *openrails.Client) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, client.ExportBillingArchive(t.Context(), &out))
	require.NotEmpty(t, out.Bytes())
	return out.Bytes()
}

func TestBillingArchive(t *testing.T) {
	t.Run("OR-J31-S01/empty-target-restore-preserves-ledger-and-resets-destination-stream", func(t *testing.T) {
		actor, provider := newPaymentWorld(t)
		source := actor.world
		status, raw := actor.request(t, "POST", "/v1/me/invoices", actor.aliceToken, saleInput(t, actor, provider), map[string]string{"Idempotency-Key": "archive-settled"})
		require.Equal(t, 201, status, string(raw))
		require.Len(t, provider.effects(t), 1)
		customer := billing.CustomerID(uuid.New())
		grants, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 12345, Source: "critical", SourceID: "archive-credit"}})
		require.NoError(t, err)
		price := hostProduct(t, source.alpha, "archive-item")
		product, err := source.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "archive-comp", DisplayName: "Archive comp", Entitlements: []string{"archive:comp"}})
		require.NoError(t, err)
		start := source.clock.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
		book := billing.DeclaredBilling{AsOf: start, AdminGrants: []billing.DeclaredAdminGrant{{Customer: customer, Product: product.ID, SourceID: "archive-import", StartsAt: start, EndsAt: ptr(start.Add(time.Hour))}}}
		_, err = source.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		book.AsOf = start.Add(time.Hour)
		book.AdminGrants[0].EndsAt = ptr(start.Add(24 * time.Hour))
		_, err = source.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		importedHistory := func(w *world) string {
			t.Helper()
			var state string
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT jsonb_agg(g ORDER BY id)::text FROM "+pgx.Identifier{w.schema, "grants"}.Sanitize()+" g WHERE merchant_id=$1 AND source_id='archive-import'", source.alpha.MerchantID().UUID()).Scan(&state))
			return state
		}
		before, err := source.alpha.ListEvents(t.Context(), billing.EventListParams{Limit: 100})
		require.NoError(t, err)
		require.NotEmpty(t, before.Events)
		archive := archiveBytes(t, source.alpha)
		var inbox *notifypg.Store
		targetWorld, target := archiveTarget(t, source, accountInbox(t, &inbox))
		receipt, err := target.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.Equal(t, source.alpha.MerchantID(), receipt.MerchantID)
		require.NotEmpty(t, receipt.Digest)
		require.Positive(t, receipt.Rows)
		require.False(t, receipt.AlreadyImported)
		require.Equal(t, importedHistory(source), importedHistory(targetWorld), "imported roots, replacements and terminations restore with immediate foreign keys")
		access, err := target.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"archive:comp"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["archive:comp"])
		local, err := targetWorld.engine.LocalMerchant(target.MerchantID())
		require.NoError(t, err)
		_, err = local.ExportNotificationAdoption(t.Context())
		require.ErrorContains(t, err, "ordering-state restoration")
		// This fixture had no notification channel before moving its book.
		require.NoError(t, local.RestoreNotificationOrdering(t.Context(), billing.NotificationAdoption{MerchantID: target.MerchantID()}))
		adoption, err := local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Empty(t, adoption.Ordering)
		require.Empty(t, adoption.Adoptions, "a financial restore must not replay old inbox messages")
		balance(t, target, customer, 12345, 0, 12345)
		after, err := target.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, after.Items, 1)
		require.Equal(t, grants[0].ID, after.Items[0].ID)
		gotPrice, err := target.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.EqualValues(t, 10_000_000, gotPrice.UnitAmount)
		events, err := target.ListEvents(t.Context(), billing.EventListParams{Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events.Events, "deployment event-log rows are not portable billing records")
		_, err = target.ListEvents(t.Context(), billing.EventListParams{After: before.NextCursor, Limit: 100})
		var expired *billing.StatusError
		require.ErrorAs(t, err, &expired)
		require.Equal(t, billing.CodeEventCursorExpired, expired.Code)
		sourceEvents, err := source.alpha.ListEvents(t.Context(), billing.EventListParams{Limit: 100})
		require.NoError(t, err)
		require.Equal(t, before.Events, sourceEvents.Events)
		replay, err := target.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.True(t, replay.AlreadyImported)
		require.Equal(t, receipt.Digest, replay.Digest)
		balance(t, target, customer, 12345, 0, 12345)
	})
	t.Run("OR-J31-S02/wrong-merchant-corrupt-and-occupied-target-refuse-atomically", func(t *testing.T) {
		source := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		_, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "archive-source"}})
		require.NoError(t, err)
		archive := archiveBytes(t, source.alpha)
		target, client := archiveTarget(t, source)
		_, err = target.beta.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.Error(t, err)
		for _, invalid := range [][]byte{archive[:len(archive)/2], bytes.Replace(archive, []byte("archive-source"), []byte("archive-forged"), 1)} {
			require.NotEqual(t, archive, invalid)
			_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(invalid))
			require.Error(t, err)
			items, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
			require.NoError(t, err)
			require.Empty(t, items.Items)
		}
		_, err = client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 700, Source: "critical", SourceID: "already-here"}})
		require.NoError(t, err)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.Error(t, err)
		balance(t, client, customer, 700, 0, 700)
		grants, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, grants.Items, 1)
		require.Equal(t, ptr("already-here"), grants.Items[0].SourceID)
	})
	t.Run("OR-J31-S03/restored-custody-cannot-charge-before-explicit-arm", func(t *testing.T) {
		source, provider := newPaymentWorld(t)
		input := saleInput(t, source, provider)
		status, raw := source.request(t, "POST", "/v1/me/invoices", source.aliceToken, input, map[string]string{"Idempotency-Key": "source-settled"})
		require.Equal(t, 201, status, string(raw))
		require.Len(t, provider.effects(t), 1)
		archive := archiveBytes(t, source.alpha)
		target, client := archiveTarget(t, source.world)
		_, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		psps, err := client.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		require.Equal(t, "acct_e2e", psps.Items[0].AccountID)
		require.False(t, psps.Items[0].Credentials["secret_key"].Configured, "credentials are never copied with the billing archive")
		// File-owned deployments change credentials through their declaration.
		require.NoError(t, target.engine.Close(t.Context()))
		cfg := target.cfg
		cfg.Merchants = slices.Clone(target.cfg.Merchants)
		// The restored merchant keeps its issuer: its return URLs' origin.
		setCfgMerchant(&cfg, "restored-source", billing.MerchantDeclaration{Issuer: alphaSite, MerchantDocument: billing.MerchantDocument{DisplayName: "Restored", PSPs: billing.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_e2e", Environment: billing.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical_fixture", PublishableKey: "pk_test_critical"}.PSPConfig()}, MerchantSettings: billing.MerchantSettings{CardVault: "stripe"}}})
		deps := target.deps
		page := declareCheckoutPage(t, &cfg, &deps)
		target.engine, err = openrails.New(t.Context(), cfg, deps)
		require.NoError(t, err)
		client, err = target.engine.Merchant(source.alpha.MerchantID())
		require.NoError(t, err)
		serveCheckoutPage(t, page, target.engine)
		customer := billing.CustomerID(uuid.MustParse(source.alice.ID))
		request := billing.CreateInvoiceParams{CustomerID: customer, Lines: input.Lines, PayURL: &billing.InvoicePayURLParams{SuccessURL: alphaSite + "/done"}}
		_, err = client.CreateInvoice(t.Context(), "destination-invoice", request)
		var held *billing.StatusError
		require.ErrorAs(t, err, &held)
		require.Equal(t, billing.CodeProviderWritesDisabled, held.Code)
		require.Len(t, provider.effects(t), 1)
		// Final source writers really stop before the operator authorizes the copy.
		require.NoError(t, source.engine.Close(context.Background()))
		hostCLI(t, target, "merchant", "writes", "full", "--merchant", "id:"+client.MerchantID().String(), "--by", "critical verified operator", "--reason", "the source is stopped")
		invoice, err := client.CreateInvoice(t.Context(), "destination-after-arm", request)
		require.NoError(t, err)
		address, err := url.Parse(*invoice.PayURL)
		require.NoError(t, err)
		payment := billing.PayInvoiceParams{ExpectedTotal: 30_000_000, Payment: billing.InvoicePaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, _, raw = hostPageRequest(t, page, "POST", "/v1/me/invoices/"+invoice.ID.String()+"/pay", address.Fragment, payment, map[string]string{"Idempotency-Key": "after-arm"})
		require.Equal(t, 200, status, string(raw))
		var completed billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &completed))
		require.Equal(t, billing.InvoiceComplete, completed.Status)
		require.Len(t, provider.effects(t), 2)
		require.EqualValues(t, 3000, provider.effects(t)[1].Amount)
	})
	t.Run("OR-J31-S04/unresolved-operation-refuses-export-without-changing-custody", func(t *testing.T) {
		source, provider := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(source.alice.ID))
		_, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "held-work"}})
		require.NoError(t, err)
		// A live gate has a request in flight.
		lease := newLease()
		syncLease(t, source.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 600}}})
		var partial bytes.Buffer
		err = source.alpha.ExportBillingArchive(t.Context(), &partial)
		require.Error(t, err)
		require.Empty(t, partial.Bytes())
		balance(t, source.alpha, customer, 400, 600, 1000)
		status, raw := source.request(t, "POST", "/v1/me/invoices", source.aliceToken, saleInput(t, source, provider), map[string]string{"Idempotency-Key": "after-failed-export"})
		require.Equal(t, 201, status, string(raw))
		require.Len(t, provider.effects(t), 1)
		syncLease(t, source.alpha, billing.SyncLeasesParams{LeaseID: lease, Close: true})
		balance(t, source.alpha, customer, 1000, 0, 1000)
		require.NotEmpty(t, archiveBytes(t, source.alpha))
	})
	t.Run("OR-J31-S06/nmi-receipts-naming-their-card-move-with-the-book", func(t *testing.T) {
		source, provider, customer := usageBillingWorld(t, false)
		// A sale and a usage collection, each holding the receipt that names the card NMI reported.
		product, err := source.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "archived-download", DisplayName: "Download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
		require.NoError(t, err)
		price, err := source.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "archived-download", UnitAmount: 9_990_000, Currency: "USD"})
		require.NoError(t, err)
		sale := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(9_990_000)), Reusable: ptr(false),
			Payment: &billing.InvoicePaymentParams{Token: provider.core.Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454", Exp: "1235"}), BillingDetails: billedTo("US", "10001")}}
		status, raw := source.request(t, http.MethodPost, "/v1/me/invoices", source.aliceToken, sale, map[string]string{"Idempotency-Key": "archived-nmi-sale"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var paid billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.InvoiceComplete, paid.Status, string(raw))
		oneUsage(t, source.alpha, usageInput(customer, "archived-work", 3))
		source.clock.Advance(time.Second)
		runUsageDue(t, startFleet(t, source.world))
		require.Eventually(t, func() bool {
			rows, err := source.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 2 && rows.Items[0].Status == billing.InvoiceComplete && rows.Items[1].Status == billing.InvoiceComplete
		}, 15*time.Second, 20*time.Millisecond)
		cards := func(w *world) map[string]string {
			rows, err := w.pool.Query(t.Context(), "SELECT intent_type, result_evidence#>>'{qualified_receipt,nmi,card_brand}', result_evidence#>>'{qualified_receipt,nmi,card_last4}' FROM "+pgx.Identifier{w.schema, "provider_intents"}.Sanitize()+" WHERE status = 'succeeded' AND result_evidence ? 'qualified_receipt'")
			require.NoError(t, err)
			defer rows.Close()
			out := map[string]string{}
			for rows.Next() {
				var kind, brand, last4 string
				require.NoError(t, rows.Scan(&kind, &brand, &last4))
				out[kind] = brand + " " + last4
			}
			require.NoError(t, rows.Err())
			return out
		}
		held := map[string]string{"nmi_sale": "mastercard 5454", "order_collection": "visa 4242"}
		require.Equal(t, held, cards(source.world))
		archive := archiveBytes(t, source.alpha)
		target, client := archiveTarget(t, source.world, func(w *world) { w.deps.NMITransport = source.deps.NMITransport })
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.Equal(t, held, cards(target))
		payment, err := client.GetPayment(t.Context(), *paid.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, "5454", *payment.Card.Last4)
	})
	t.Run("OR-J31-S09/a-paid-stripe-hosted-page-invoice-moves-with-the-book", func(t *testing.T) {
		source, provider := newPaymentWorld(t)
		provider.core.SendWebhooksTo(source.server.URL+"/v1/webhooks/stripe/acct_e2e", "whsec_critical_fixture")
		sale := saleInput(t, source, provider)
		hosted := billing.CreateMyInvoiceParams{Lines: sale.Lines, ExpectedTotal: sale.ExpectedTotal, Payment: &billing.InvoicePaymentParams{ReturnURL: alphaSite + "/paid"}}
		status, raw := source.request(t, http.MethodPost, "/v1/me/invoices", source.aliceToken, hosted, map[string]string{"Idempotency-Key": "archived-hosted"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.NotNil(t, invoice.Payment.NextAction)
		require.Equal(t, "redirect_to_url", invoice.Payment.NextAction.Type)
		// The attempt keeps Stripe's page in its rail state: the session and its URL.
		_, err := provider.core.CompleteCheckoutSession(t.Context(), strings.TrimPrefix(*invoice.Payment.NextAction.URL, stripemock.CheckoutURL))
		require.NoError(t, err)
		var paid *billing.Invoice
		require.Eventually(t, func() bool {
			paid, err = source.alpha.GetInvoice(t.Context(), invoice.ID)
			return err == nil && paid.Status == billing.InvoiceComplete
		}, 20*time.Second, 20*time.Millisecond)
		require.NotNil(t, paid.Payment.PaymentID)
		archive := archiveBytes(t, source.alpha)
		_, client := archiveTarget(t, source.world)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		restored, err := client.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceComplete, restored.Status)
		require.Equal(t, paid.Payment.PaymentID, restored.Payment.PaymentID)
	})
	t.Run("OR-J31-S10/a-restored-solana-pull-runs-at-its-due-time", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		// The source's poller settles the subscribe's reference: a pending one
		// stays with the deployment that watches the chain.
		quietFleet(t, s.world)
		require.Eventually(t, func() bool {
			s.clock.Advance(time.Second)
			var pending int
			err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{s.schema, "solana_pay_references"}.Sanitize()+" WHERE status = 'pending'").Scan(&pending)
			return err == nil && pending == 0
		}, 20*time.Second, 100*time.Millisecond, "the poller confirms the landed subscribe")
		archive := archiveBytes(t, s.alpha)
		target, client := archiveTarget(t, s.world, func(w *world) {
			w.clock.Advance(s.clock.Now().Sub(w.clock.Now()))
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{SolanaRPCURL: s.wire.server.URL}
			w.cfg.Solana = billing.SolanaConfig{Crankers: []billing.SolanaCrankerConfig{{Mode: "local_keypair", PrivateKey: s.cranker.PrivateKey.String()}}}
		})
		s.node.SetClock(target.clock.Now)
		_, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		// The merchant's configuration names the same treasury.
		require.NoError(t, target.engine.Close(t.Context()))
		cfg := target.cfg
		cfg.Merchants = slices.Clone(target.cfg.Merchants)
		// The restored merchant keeps its issuer: its return URLs' origin.
		setCfgMerchant(&cfg, "restored-source", billing.MerchantDeclaration{Issuer: alphaSite, MerchantDocument: billing.MerchantDocument{DisplayName: "Restored", PSPs: billing.PSPs{"solana": openrails.SolanaPSP{Environment: billing.EnvironmentDevnet, Treasury: s.treasury.PublicKey().String(), RPCURL: s.wire.server.URL}.PSPConfig()}}})
		target.engine, err = openrails.New(t.Context(), cfg, target.deps)
		require.NoError(t, err)
		client, err = target.engine.Merchant(s.alpha.MerchantID())
		require.NoError(t, err)
		require.NoError(t, s.engine.Close(context.Background()))
		fleet := quietFleet(t, target)
		require.NotEmpty(t, operationJobs(t, fleet), "the restore queues the pull's wake")
		// Pause the workers at each clock advance. River's JobRetry leaves a
		// running wake alone, so it could otherwise snooze against the old clock.
		require.NoError(t, fleet.Stop(t.Context()))
		sends := s.wire.sends()
		pullAt := due.Add(pullMargin)
		advanceTo(target, pullAt)
		wakeOperations(t, fleet)
		require.NoError(t, fleet.Start(t.Context()))
		retryAt := pullAt.Add(5 * time.Minute)
		intents := pgx.Identifier{target.schema, "provider_intents"}.Sanitize()
		require.Eventually(t, func() bool {
			var reason string
			err := target.pool.QueryRow(t.Context(), "SELECT last_failure_reason FROM "+intents+" WHERE intent_type = 'solana_pull' AND status = 'pending' AND next_attempt_at = $1", retryAt).Scan(&reason)
			return err == nil && strings.Contains(reason, "readonly (billing restore)")
		}, 20*time.Second, 20*time.Millisecond, "the due pull is parked while the restored merchant is read-only")
		require.NoError(t, fleet.Stop(t.Context()))
		require.Equal(t, sends, s.wire.sends(), "a restored pull sends nothing before the operator arms")
		hostCLI(t, target, "merchant", "writes", "full", "--merchant", "id:"+client.MerchantID().String(), "--by", "critical verified operator", "--reason", "the source is stopped")
		advanceTo(target, retryAt)
		wakeOperations(t, fleet)
		require.NoError(t, fleet.Start(t.Context()))
		require.Eventually(t, func() bool {
			current, err := client.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.Equal(retryAt.Add(168*time.Hour))
		}, 20*time.Second, 20*time.Millisecond, "the restored pull renews the period at its due time")
		require.Equal(t, sends+1, s.wire.sends())
		payments, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2, "the restored subscribe payment and one renewal")
		require.EqualValues(t, 20_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	})
}
