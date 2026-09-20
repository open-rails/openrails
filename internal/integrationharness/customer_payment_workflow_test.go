//go:build integration

package integrationharness

import (
	"context"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/pkg/merchant"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/app"
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
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			_ = r.Body.Close()
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			form, err := url.ParseQuery(string(raw))
			require.NoError(t, err)
			if form.Get("type") == "sale" {
				mu.Lock()
				forms = append(forms, form)
				mu.Unlock()
			}
		}
		gateway.serve(w, r)
	}))
	t.Cleanup(wire.Close)
	standalone := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL} }))
	cp := embcp.Get(standalone.App())
	authn, err := orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), dbtest.TestMerchantID.String())
	require.NoError(t, err)
	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(), DelegatedAuthenticator: authn,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	app.HostGraph(rt).Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)
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
			f := h.SeedPastDueInvoiceForCustomer(app.HostGraph(rt).Runtime, dbtest.TestMerchantID, customer, currency, amount)
			// This method was saved without an approved unscheduled agreement. A
			// customer-present payment must establish one before future MIT.
			_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.payment_methods SET stored_credential_unscheduled_ref='' WHERE id=$1`, f.Method)
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
			for _, bad := range []string{"", "invalid", standalone.Token} {
				_, err := newClient(bad).PayInvoiceNow(ctx, request)
				require.Error(t, err, "absent, invalid and merchant credentials cannot become a customer action")
			}
			if mode == "embedded" {
				owner, err := rt.Client()
				require.NoError(t, err)
				_, err = owner.PayInvoiceNow(billingauth.SetUserContext(ctx, billingauth.UserContext{UserID: user.ID}), request)
				require.Error(t, err, "ambient host user cannot turn the default owner into CIT")
			}
			client := newClient(token)
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
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id=$1`, f.Method).Scan(&anchor))
			require.Equal(t, gateway.Sales()[len(gateway.Sales())-1].TransactionID, anchor)
			replay, err := client.PayInvoiceNow(ctx, request)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.Equal(t, paid.Operation, replay.Operation)
			require.Equal(t, before+1, gateway.SaleAttempts())
			read, err := client.GetMyInvoice(ctx, f.Invoice)
			require.NoError(t, err)
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
		})
	}
}
