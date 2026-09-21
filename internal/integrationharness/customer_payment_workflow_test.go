//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/pkg/merchant"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// A real AuthKit user drives the same Client over in-process and mounted HTTP
// transports. Provider writes are local HTTP; receipt qualification and all
// financial effects use PostgreSQL and the production operation runner.
func TestCustomerInvoicePaymentClientWorkflow(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	gateway := NewFakeNMIGateway(t)
	var mu sync.Mutex
	var forms []url.Values
	type recurringObligation struct{ vault, billing, amount, currency, next string }
	obligations := map[string]recurringObligation{}
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/subscriptions/") {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/subscriptions/")+len("/subscriptions/"):]
			mu.Lock()
			obligation, found := obligations[id]
			mu.Unlock()
			if found {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "amount": obligation.amount, "customer_vault_id": obligation.vault, "delayed_condition": "active", "paused_subscription": "0", "next_billing_date": obligation.next, "plan": map[string]any{"id": "fixed-days", "plan_amount": obligation.amount, "plan_payments": "0", "day_frequency": "30"}})
				return
			}
		}
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			_ = r.Body.Close()
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			form, err := url.ParseQuery(string(raw))
			require.NoError(t, err)
			if form.Get("type") == "sale" || form.Get("recurring") == "rebill_subscription" {
				mu.Lock()
				forms = append(forms, form)
				mu.Unlock()
			}
			if form.Get("recurring") == "rebill_subscription" {
				mu.Lock()
				obligation, found := obligations[form.Get("subscription_id")]
				mu.Unlock()
				if !found {
					http.Error(w, "unknown recurring obligation", http.StatusNotFound)
					return
				}
				require.Equal(t, obligation.vault, form.Get("customer_vault_id"))
				require.Equal(t, obligation.billing, form.Get("billing_id"))
				// NMI's recurring engine supplies its stored amount/currency;
				// the actual request above is retained before this fake dispatch.
				dispatch := url.Values{"type": {"sale"}, "orderid": form["orderid"], "customer_vault_id": {obligation.vault}, "amount": {obligation.amount}, "currency": {obligation.currency}}
				r.Body = io.NopCloser(strings.NewReader(dispatch.Encode()))
				r.Form, r.PostForm = nil, nil
			}
		}
		gateway.serve(w, r)
	}))
	t.Cleanup(wire.Close)
	standalone := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL} }))
	owned := standalone.ProvisionOwnedMerchant("customer-recovery-" + uuid.NewString()[:8])
	machine := standalone.RegisterRemoteApplication("payment-machine-"+uuid.NewString()[:8], owned.MerchantSlug, controlplane.MerchantRoleOwner)
	cp := embcp.Get(standalone.App())
	authn, err := orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(), DelegatedAuthenticator: authn,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	app.HostGraph(rt).Runtime.SetConfiguredMerchant(owned.MerchantID)
	handler, err := rt.Handler(embed.MountOptions{RouteSets: []embed.RouteSet{embed.RouteSetCustomer}})
	require.NoError(t, err, "mount inherits the runtime's verifier")
	mounted := httptest.NewServer(handler)
	t.Cleanup(mounted.Close)
	for _, mode := range []string{"embedded", "http"} {
		t.Run(mode, func(t *testing.T) {
			user, err := cp.Core().CreateUser(ctx, mode+uuid.NewString()+"@example.test", "payer"+uuid.NewString()[:8])
			require.NoError(t, err)
			token, _, err := cp.Core().MintAccessToken(ctx, user.ID, nil)
			require.NoError(t, err)
			customer := uuid.MustParse(user.ID)
			currency, amount, providerAmount := "USD", int64(50_000), "0.05"
			if mode == "http" {
				currency, amount, providerAmount = "JPY", 30_001, "4.00"
			}
			f := h.SeedPastDueInvoiceForCustomer(app.HostGraph(rt).Runtime, owned.MerchantID, customer, currency, amount)
			// This method was saved without an approved unscheduled agreement. A
			// customer-present payment must establish one before future MIT.
			_, err = h.sharedPool().Exec(ctx, `UPDATE billing.payment_methods SET stored_credential_unscheduled_ref='' WHERE id=$1`, f.Method)
			require.NoError(t, err)
			newClient := func(credential string) *openrails.Client {
				opts := []openrails.ClientOption{openrails.WithMerchantID(f.Merchant), openrails.WithTokenProvider(func(context.Context) (string, error) { return credential, nil })}
				var c *openrails.Client
				var err error
				if mode == "embedded" {
					c, err = rt.Client(opts...)
				} else {
					c, err = openrails.NewRemote(mounted.URL, opts...)
				}
				require.NoError(t, err)
				return c
			}
			request := openrails.PayInvoiceNowRequest{InvoiceID: f.Invoice, PaymentMethodID: openrails.PaymentMethodID(f.Method), IdempotencyKey: uuid.NewString()}
			for _, bad := range []string{"", "invalid", "in-process-host", standalone.Token, machine.Token} {
				_, err := newClient(bad).PayInvoiceNow(ctx, request)
				require.Error(t, err, "absent, invalid and merchant credentials cannot become a customer action")
			}
			if mode == "embedded" {
				_, err := newClient("in-process-host").CreateProduct(ctx, openrails.CreateProductRequest{Key: "forged-" + uuid.NewString(), DisplayName: "must not write"})
				require.Error(t, err, "forwarded well-known string cannot become merchant owner")
				owner, err := rt.Client()
				require.NoError(t, err)
				_, err = owner.CreateProduct(ctx, openrails.CreateProductRequest{Key: "owner-" + uuid.NewString(), DisplayName: "Default host authority"})
				require.NoError(t, err, "default host client retains ordinary merchant authority")
				_, err = owner.PayInvoiceNow(billingauth.SetUserContext(ctx, billingauth.UserContext{UserID: user.ID}), request)
				require.Error(t, err, "ambient host user cannot turn the default owner into CIT")
			}
			client := newClient(token)
			// The same actual embedded mount and socket mount must reject
			// conflicting Client/runtime constraints before reading this payer.
			for _, binding := range []struct {
				name, header string
				configured   merchant.ID
				status       int
			}{
				{"wrong client", uuid.NewString(), f.Merchant, 409},
				{"wrong runtime", f.Merchant.String(), merchant.ID(uuid.New()), 409},
				{"invalid header", "invalid", f.Merchant, 400},
			} {
				func() {
					runtime := app.HostGraph(rt).Runtime
					runtime.SetConfiguredMerchant(binding.configured)
					defer runtime.SetConfiguredMerchant(f.Merchant)
					wire, err := http.NewRequestWithContext(ctx, http.MethodGet, mounted.URL+"/v1/me/invoices/"+f.Invoice.String(), nil)
					require.NoError(t, err)
					wire.Header.Set("Authorization", "Bearer "+token)
					wire.Header.Set(merchant.BindingHeader, binding.header)
					if mode == "embedded" {
						response := httptest.NewRecorder()
						handler.ServeHTTP(response, wire)
						require.Equal(t, binding.status, response.Code, binding.name+response.Body.String())
					} else {
						response, err := http.DefaultClient.Do(wire)
						require.NoError(t, err)
						body, err := io.ReadAll(response.Body)
						require.NoError(t, err)
						require.NoError(t, response.Body.Close())
						require.Equal(t, binding.status, response.StatusCode, binding.name+string(body))
					}
				}()
			}
			declinedRequest := request
			gateway.SetMode(NMISaleDecline)
			_, declinedError := client.PayInvoiceNow(ctx, declinedRequest)
			var invoiceRefusal *openrails.StatusError
			require.ErrorAs(t, declinedError, &invoiceRefusal)
			require.Equal(t, 402, invoiceRefusal.Status)
			require.Equal(t, openrails.CodeCardDeclined, invoiceRefusal.Code)
			require.NotEmpty(t, invoiceRefusal.Metadata["operation_id"])
			declinedInvoice, err := client.GetMyInvoice(ctx, f.Invoice)
			require.NoError(t, err)
			require.True(t, declinedInvoice.Recovery.Retryable)
			require.Equal(t, invoiceRefusal.Metadata["decline_reason"], declinedInvoice.Recovery.LastFailureReason)
			require.NotEmpty(t, declinedInvoice.Recovery.LastFailureReason)
			require.Positive(t, declinedInvoice.CollectionFailureCount)
			require.NotNil(t, declinedInvoice.NextCollectionAttemptAt)

			gateway.SetMode(NMISaleApprove)
			request.IdempotencyKey = uuid.NewString()
			before := gateway.SaleAttempts()
			if mode == "http" {
				gateway.SetMode(NMISaleUncertain)
				gateway.SetVisible(false)
			}
			paid, err := client.PayInvoiceNow(ctx, request)
			require.NoError(t, err)
			if mode == "http" {
				require.True(t, paid.Operation.Unresolved())
				pending, err := client.GetMyInvoice(ctx, f.Invoice)
				require.NoError(t, err)
				require.Equal(t, paid.Operation, *pending.Recovery.Operation)
				require.False(t, pending.Recovery.Retryable)
				replay, err := client.PayInvoiceNow(ctx, request)
				require.NoError(t, err)
				require.True(t, replay.Replayed)
				require.Equal(t, paid.Operation, replay.Operation)
				competing := request
				competing.IdempotencyKey = uuid.NewString()
				_, err = client.PayInvoiceNow(ctx, competing)
				require.ErrorIs(t, err, openrails.ErrConflict)
				require.Equal(t, before+1, gateway.SaleAttempts())
				gateway.SetVisible(true)
				gateway.SetMode(NMISaleApprove)
				h.MakeOperationDue(paid.Operation.ID)
				runtime := app.HostGraph(rt).Runtime
				require.NoError(t, runtime.DB.RunInMerchantConn(merchant.WithID(ctx, f.Merchant), func(c context.Context) error { _, err := runtime.IntentRunner().RunVerifyOnce(c); return err }))
				paid, err = client.PayInvoiceNow(ctx, request)
				require.NoError(t, err)
			}
			require.Equal(t, "succeeded", paid.Operation.Status)
			require.Equal(t, "paid", paid.Invoice.Status)
			require.Equal(t, before+1, gateway.SaleAttempts())
			mu.Lock()
			form := forms[len(forms)-1]
			mu.Unlock()
			require.Equal(t, "customer", form.Get("initiated_by"))
			require.Equal(t, providerAmount, form.Get("amount"))
			require.Equal(t, currency, form.Get("currency"))
			require.Equal(t, "stored", form.Get("stored_credential_indicator"))
			require.Empty(t, form.Get("initial_transaction_id"))
			var anchor string
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_unscheduled_ref FROM billing.payment_methods WHERE id=$1`, f.Method).Scan(&anchor))
			require.Equal(t, gateway.Sales()[len(gateway.Sales())-1].TransactionID, anchor)
			replay, err := client.PayInvoiceNow(ctx, request)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.Equal(t, paid.Operation, replay.Operation)
			require.Equal(t, before+1, gateway.SaleAttempts())
			_, err = client.PayInvoiceNow(ctx, declinedRequest)
			var oldInvoiceRefusal *openrails.StatusError
			require.ErrorAs(t, err, &oldInvoiceRefusal)
			require.Equal(t, 402, oldInvoiceRefusal.Status)
			require.Equal(t, invoiceRefusal.Code, oldInvoiceRefusal.Code)
			require.Equal(t, invoiceRefusal.Metadata, oldInvoiceRefusal.Metadata)
			require.Equal(t, before+1, gateway.SaleAttempts(), "old refusal cannot charge or disturb paid invoice")
			read, err := client.GetMyInvoice(ctx, f.Invoice)
			require.NoError(t, err)
			require.Empty(t, read.Recovery.LastFailureReason)
			require.Equal(t, "paid", read.Status)
			require.False(t, read.Recovery.Retryable)
			require.Equal(t, 1, h.OwedPaymentTransfers(customer))
			// The newly qualified customer authorization makes a later real
			// scheduled MIT possible; no test-only database anchor is supplied.
			runtime := app.HostGraph(rt).Runtime
			mctx := merchant.WithID(ctx, f.Merchant)
			require.NoError(t, runtime.DB.RunInMerchantConn(mctx, func(c context.Context) error {
				payer := identity.CustomerID(customer)
				if err := runtime.MoneyService.SetInvoiceCollectionPaymentMethod(c, payer, currency, f.Method); err != nil {
					return err
				}
				if _, err := runtime.MoneyService.AccrueOwed(c, payer, currency, "next-window", uuid.NewString(), amount); err != nil {
					return err
				}
				if _, err := runtime.MoneyService.FinalizeInvoice(c, payer, currency, time.Now().Add(-time.Minute), time.Now().Add(time.Hour)); err != nil {
					return err
				}
				_, err := runtime.MoneyService.ChargeOutstanding(c, runtime.IntentRunner(), 0)
				return err
			}))
			require.Equal(t, before+2, gateway.SaleAttempts())
			mu.Lock()
			later := forms[len(forms)-1]
			mu.Unlock()
			require.Equal(t, "merchant", later.Get("initiated_by"))
			require.Equal(t, "used", later.Get("stored_credential_indicator"))
			require.Equal(t, anchor, later.Get("initial_transaction_id"))

			other, err := cp.Core().CreateUser(ctx, "other"+uuid.NewString()+"@example.test", "other"+uuid.NewString()[:8])
			require.NoError(t, err)
			wrong, _, err := cp.Core().MintAccessToken(ctx, other.ID, nil)
			require.NoError(t, err)
			_, err = newClient(wrong).PayInvoiceNow(ctx, request)
			require.ErrorIs(t, err, openrails.ErrNotFound)
			changed := request
			changed.PaymentMethodID = openrails.PaymentMethodID(uuid.New())
			_, err = client.PayInvoiceNow(ctx, changed)
			require.ErrorIs(t, err, openrails.ErrConflict)
			require.Equal(t, before+2, gateway.SaleAttempts())
			// The same verified-customer Client now recovers a subscription;
			// no merchant service credential or HTTP wrapper participates.
			product, price, subscription := uuid.New(), uuid.New(), uuid.New()
			periodEnd := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
			psp := h.ArmLoopbackNMI(runtime, f.Merchant)
			billing, railSub := "billing-"+f.Method.String(), "subscription-"+subscription.String()
			_, err = h.sharedPool().Exec(ctx, `UPDATE billing.payment_methods SET rail_method_ref=$2,rebill_driver='openrails',stored_credential_recurring_ref='approved-recurring' WHERE id=$1`, f.Method, billing)
			require.NoError(t, err)
			_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Recovery','{"paid":null}')`, product, f.Merchant.UUID(), product.String())
			require.NoError(t, err)
			_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,9990000,'USD',720,true)`, price, f.Merchant.UUID(), product, price.String())
			require.NoError(t, err)
			_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,rail_subscription_id,payment_method_id,status,started_at,current_period_starts_at,current_period_ends_at,next_retry_at,retry_attempts,entitlements_spec_snapshot) VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,$8,'past_due',$9,$9,$10,$11,1,'{"paid":null}')`, subscription, f.Merchant.UUID(), customer, product, price, psp, railSub, f.Method, periodEnd.Add(-30*24*time.Hour), periodEnd, time.Now().Add(48*time.Hour))
			require.NoError(t, err)
			mu.Lock()
			obligations[railSub] = recurringObligation{f.Vault, billing, "9.99", "USD", periodEnd.Add(30 * 24 * time.Hour).Format("2006-01-02")}
			mu.Unlock()
			retry := openrails.RetrySubscriptionNowRequest{SubscriptionID: openrails.SubscriptionID(subscription), IdempotencyKey: uuid.NewString()}
			due, err := client.GetMySubscription(ctx, retry.SubscriptionID)
			require.NoError(t, err)
			require.True(t, due.Recovery.Retryable)
			refusedRetry := retry
			gateway.SetMode(NMISaleDecline)
			_, err = client.RetrySubscriptionNow(ctx, refusedRetry)
			var subscriptionRefusal *openrails.StatusError
			require.ErrorAs(t, err, &subscriptionRefusal)
			require.Equal(t, 402, subscriptionRefusal.Status)
			require.Equal(t, openrails.CodeCardDeclined, subscriptionRefusal.Code)
			declinedSubscription, err := client.GetMySubscription(ctx, retry.SubscriptionID)
			require.NoError(t, err)
			require.True(t, declinedSubscription.Recovery.Retryable)
			require.Equal(t, subscriptionRefusal.Metadata["decline_reason"], declinedSubscription.Recovery.LastFailureReason)
			require.NotEmpty(t, declinedSubscription.Recovery.LastFailureReason)
			require.NotNil(t, declinedSubscription.RetryAttempts)
			require.Greater(t, *declinedSubscription.RetryAttempts, *due.RetryAttempts)
			require.NotNil(t, declinedSubscription.NextRetryAt)
			replacementPSP := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "replacement-nmi-"+uuid.NewString())
			for _, replacement := range []struct {
				account   uuid.UUID
				reference string
			}{
				{psp, railSub + "-replacement"}, {replacementPSP, railSub},
			} {
				tx, err := h.sharedPool().Begin(ctx)
				require.NoError(t, err)
				_, err = tx.Exec(ctx, `UPDATE openrails.payment_methods SET psp_id=$2 WHERE id=$1`, f.Method, replacement.account)
				require.NoError(t, err)
				_, err = tx.Exec(ctx, `UPDATE openrails.subscriptions SET psp_id=$2,rail_subscription_id=$3 WHERE id=$1`, subscription, replacement.account, replacement.reference)
				require.NoError(t, err)
				require.NoError(t, tx.Commit(ctx))
				rebound, err := client.GetMySubscription(ctx, retry.SubscriptionID)
				require.NoError(t, err, "a valid prior account/refusal cannot break current readback")
				require.Empty(t, rebound.Recovery.LastFailureReason)
			}
			tx, err := h.sharedPool().Begin(ctx)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `UPDATE openrails.payment_methods SET psp_id=$2 WHERE id=$1`, f.Method, psp)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `UPDATE openrails.subscriptions SET psp_id=$2,rail_subscription_id=$3 WHERE id=$1`, subscription, psp, railSub)
			require.NoError(t, err)
			require.NoError(t, tx.Commit(ctx))

			gateway.SetMode(NMISaleApprove)
			retry.IdempotencyKey = uuid.NewString()
			recovered, err := client.RetrySubscriptionNow(ctx, retry)
			require.NoError(t, err)
			require.Equal(t, "succeeded", recovered.Operation.Status)
			require.Equal(t, "active", recovered.Subscription.Status)
			require.True(t, recovered.Subscription.CurrentPeriodEndsAt.Equal(periodEnd.Add(30*24*time.Hour)))
			mu.Lock()
			rebill := forms[len(forms)-1]
			mu.Unlock()
			require.Equal(t, "rebill_subscription", rebill.Get("recurring"))
			require.Equal(t, "customer", rebill.Get("initiated_by"))
			require.Equal(t, "approved-recurring", rebill.Get("initial_transaction_id"))
			same, err := client.RetrySubscriptionNow(ctx, retry)
			require.NoError(t, err)
			require.True(t, same.Replayed)
			require.Equal(t, recovered.Operation, same.Operation)
			_, err = client.RetrySubscriptionNow(ctx, refusedRetry)
			var oldSubscriptionRefusal *openrails.StatusError
			require.ErrorAs(t, err, &oldSubscriptionRefusal)
			require.Equal(t, 402, oldSubscriptionRefusal.Status)
			require.Equal(t, subscriptionRefusal.Code, oldSubscriptionRefusal.Code)
			require.Equal(t, subscriptionRefusal.Metadata, oldSubscriptionRefusal.Metadata)
			after, err := client.GetMySubscription(ctx, retry.SubscriptionID)
			require.NoError(t, err)
			require.Equal(t, "active", after.Status)
			require.Empty(t, after.Recovery.LastFailureReason)
			require.True(t, after.CurrentPeriodEndsAt.Equal(*recovered.Subscription.CurrentPeriodEndsAt))
			require.Equal(t, before+4, gateway.SaleAttempts(), fmt.Sprintf("%s invoice/CIT/MIT/rebill workflow", mode))
			// Stripe customer-action continuation is explicitly unsupported in this
			// release; a verified customer cannot accidentally invoke off-session MIT.
			unsupported := h.SeedPastDueInvoiceForCustomer(app.HostGraph(rt).Runtime, owned.MerchantID, customer, "USD", 50_000)
			// This is an explicitly customer-collected invoice, not work for the
			// later automatic collection leg of this shared workflow.
			_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.invoices SET collection_method='send_invoice' WHERE id=$1`, unsupported.Invoice)
			require.NoError(t, err)
			stripePSP := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "stripe")
			_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.payment_methods SET rail='stripe',psp_id=$2,rail_customer_ref=$3,rail_method_ref=$3 WHERE id=$1`, unsupported.Method, stripePSP, "pm_unsupported_"+unsupported.Method.String())
			require.NoError(t, err)
			_, err = client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{InvoiceID: unsupported.Invoice, PaymentMethodID: openrails.PaymentMethodID(unsupported.Method), IdempotencyKey: uuid.NewString()})
			var unsupportedError *openrails.StatusError
			require.ErrorAs(t, err, &unsupportedError)
			require.Equal(t, 400, unsupportedError.Status)
			require.Equal(t, "customer_payment_unsupported", unsupportedError.Code)
			require.Equal(t, before+4, gateway.SaleAttempts())
			var attempts int
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_payments WHERE invoice_id=$1`, unsupported.Invoice).Scan(&attempts))
			require.Zero(t, attempts, "unsupported customer payment refuses before durable charge admission")

		})
	}
}
