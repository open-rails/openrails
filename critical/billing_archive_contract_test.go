//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/internal/stripemock"
	"maps"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func archiveTarget(t *testing.T, source *world) (*world, *openrails.Client) {
	t.Helper()
	target := newWorld(t, func(w *world) {
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		w.deps.StripeTransport = source.deps.StripeTransport
	})
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
		status, raw := actor.request(t, "POST", "/v1/me/orders", actor.aliceToken, saleInput(t, actor, provider), map[string]string{"Idempotency-Key": "archive-settled"})
		require.Equal(t, 201, status, string(raw))
		require.Len(t, provider.effects(t), 1)
		customer := billing.CustomerID(uuid.New())
		grants, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 12345, Source: "critical", SourceID: "archive-credit"}})
		require.NoError(t, err)
		price := hostProduct(t, source.alpha, "archive-item")
		before, err := source.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		require.NotEmpty(t, before.Events)
		archive := archiveBytes(t, source.alpha)
		_, target := archiveTarget(t, source)
		receipt, err := target.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.Equal(t, source.alpha.MerchantID(), receipt.MerchantID)
		require.NotEmpty(t, receipt.Digest)
		require.Positive(t, receipt.Rows)
		require.False(t, receipt.AlreadyImported)
		balance(t, target, customer, 12345, 0, 12345)
		after, err := target.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, after.Items, 1)
		require.Equal(t, grants[0].ID, after.Items[0].ID)
		gotPrice, err := target.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.EqualValues(t, 10_000_000, gotPrice.UnitAmount)
		events, err := target.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events.Events, "deployment event-log rows are not portable billing records")
		_, err = target.ListEvents(t.Context(), billing.EventQuery{After: before.NextCursor, Limit: 100})
		var expired *billing.StatusError
		require.ErrorAs(t, err, &expired)
		require.Equal(t, billing.CodeEventCursorExpired, expired.Code)
		sourceEvents, err := source.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
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
		status, raw := source.request(t, "POST", "/v1/me/orders", source.aliceToken, input, map[string]string{"Idempotency-Key": "source-settled"})
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
		cfg.Merchants = maps.Clone(target.cfg.Merchants)
		cfg.Merchants["restored-source"] = openrails.MerchantDeclaration{DisplayName: "Restored", PSPs: openrails.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_e2e", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical_fixture", PublishableKey: "pk_test_critical"}.PSPConfig()}, MerchantSettings: billing.MerchantSettings{CardVault: "stripe"}}
		target.engine, err = openrails.New(t.Context(), cfg, target.deps)
		require.NoError(t, err)
		client, err = target.engine.Merchant(source.alpha.MerchantID())
		require.NoError(t, err)
		page := hostCheckoutPage(t, target)
		customer := billing.CustomerID(uuid.MustParse(source.alice.ID))
		request := billing.CreateOrderParams{CustomerID: customer, Lines: input.Lines, IdempotencyKey: "destination-order", PayURL: &billing.OrderPayURLParams{SuccessURL: "https://critical.invalid/done"}}
		_, err = client.CreateOrder(t.Context(), request)
		var held *billing.StatusError
		require.ErrorAs(t, err, &held)
		require.Equal(t, billing.CodeProviderWritesDisabled, held.Code)
		require.Len(t, provider.effects(t), 1)
		// Final source writers really stop before the operator authorizes the copy.
		require.NoError(t, source.engine.Close(context.Background()))
		hostCLI(t, target, "merchant", "arm", "--merchant", "id:"+client.MerchantID().String(), "--by", "critical verified operator")
		request.IdempotencyKey = "destination-after-arm"
		order, err := client.CreateOrder(t.Context(), request)
		require.NoError(t, err)
		address, err := url.Parse(*order.PayURL)
		require.NoError(t, err)
		payment := billing.PayOrderParams{ExpectedTotal: 30_000_000, Payment: billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, _, raw = hostPageRequest(t, page, "POST", "/v1/me/orders/"+order.ID.String()+"/pay", address.Fragment, payment, map[string]string{"Idempotency-Key": "after-arm"})
		require.Equal(t, 200, status, string(raw))
		var completed billing.Order
		require.NoError(t, json.Unmarshal(raw, &completed))
		require.Equal(t, billing.OrderComplete, completed.Status)
		require.Len(t, provider.effects(t), 2)
		require.EqualValues(t, 3000, provider.effects(t)[1].Amount)
	})
	t.Run("OR-J31-S04/unresolved-operation-refuses-export-without-changing-custody", func(t *testing.T) {
		source, provider := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(source.alice.ID))
		_, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "held-work"}})
		require.NoError(t, err)
		admitted, err := source.alpha.Admit(t.Context(), []billing.AdmitParams{{RequestID: "unfinished", CustomerID: customer, Currency: "USD", Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, EstimatedAmount: 600, ExpiresAt: ptr(source.clock.Now().Add(time.Hour))}})
		require.NoError(t, err)
		require.Len(t, admitted, 1)
		require.True(t, admitted[0].Allowed())
		var partial bytes.Buffer
		err = source.alpha.ExportBillingArchive(t.Context(), &partial)
		require.Error(t, err)
		require.Empty(t, partial.Bytes())
		balance(t, source.alpha, customer, 400, 600, 1000)
		hold, err := source.alpha.GetAdmission(t.Context(), "unfinished")
		require.NoError(t, err)
		require.Equal(t, ptr(billing.AdmissionOpen), hold.State)
		status, raw := source.request(t, "POST", "/v1/me/orders", source.aliceToken, saleInput(t, source, provider), map[string]string{"Idempotency-Key": "after-failed-export"})
		require.Equal(t, 201, status, string(raw))
		require.Len(t, provider.effects(t), 1)
		released, err := source.alpha.ReleaseAdmissions(t.Context(), []string{"unfinished"})
		require.NoError(t, err)
		require.Len(t, released, 1)
		require.NoError(t, released[0].Err())
		balance(t, source.alpha, customer, 1000, 0, 1000)
		require.NotEmpty(t, archiveBytes(t, source.alpha))
	})

}
