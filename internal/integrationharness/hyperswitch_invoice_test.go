//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authhttp"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	embedauth "github.com/open-rails/openrails/embed/authkit"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Ordinary deployment coverage uses loopback custody and PSP boundaries. The
// explicit browser qualification separately proves real vendor interpolation.
func TestHyperSwitchInvoiceClientWorkflow(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCaptureFixture(t)
	original := g.server.Config.Handler
	var mu sync.Mutex
	forms := map[string]map[string]string{}
	g.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/proxy":
			require.Equal(t, "api-key=capture-fixture-key", r.Header.Get("Authorization"))
			var input struct {
				Token     string            `json:"token"`
				TokenType string            `json:"token_type"`
				Form      map[string]string `json:"request_body"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			require.Equal(t, "payment_method_id", input.TokenType)
			require.True(t, strings.HasPrefix(input.Token, "method-capture-"))
			require.Equal(t, "{{$card_number}}", input.Form["ccnumber"])
			require.Equal(t, "{{$card_expiry_mmyy}}", input.Form["ccexp"])
			require.Equal(t, "invoice-key", input.Form["security_key"])
			order := input.Form["orderid"]
			require.NotContains(t, forms, order, "each operation sends once")
			forms[order] = input.Form
			_, _ = fmt.Fprint(w, `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":"invoice_receipt"},"status_code":200,"response_headers":{}}`)
		case r.Method == "POST" && r.URL.Path == "/nmi":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "invoice-key", r.Form.Get("security_key"))
			_, _ = fmt.Fprint(w, "<nm_response>")
			if forms[r.Form.Get("order_id")] != nil {
				_, _ = fmt.Fprintf(w, `<transaction><transaction_id>invoice_receipt</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction>`, r.Form.Get("order_id"))
			}
			_, _ = fmt.Fprint(w, "</nm_response>")
		case r.Method == "GET" && r.URL.Path == "/nmi/payments/invoice_receipt":
			require.Equal(t, "invoice-key", r.Header.Get("Authorization"))
			_, _ = fmt.Fprint(w, `{"id":"invoice_receipt","object":"transaction","response":"1","amount":"5.00","currency":"USD","actions":[{"id":"invoice_sale","type":"sale","amount":"5.00","success":true,"response":"1"}]}`)
		default:
			original.ServeHTTP(w, r)
		}
	})
	var delegated billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: g.server.URL, SDKURL: g.server.URL + "/sdk.js"}
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: g.server.URL + "/nmi"}
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			if delegated == nil {
				return nil, billingauth.ErrUnauthenticated
			}
			return delegated.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("hs-invoice-" + uuid.NewString()[:8])
	rt := surface.App().Runtime
	core := operator.Get(surface.App()).Core()
	auth, err := authhttp.New(core, authhttp.Config{DirectPeerIP: true})
	require.NoError(t, err)
	t.Cleanup(auth.Close)
	delegated, err = embedauth.NewDelegatedAuthenticator(auth.Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	user, err := core.CreateUser(ctx, "invoice-"+uuid.NewString()+"@example.test", "invoice"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.NoError(t, core.MarkEmailVerified(ctx, user.ID))
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "nmi")
	custodian := uuid.New()
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"public_api_key":"capture-public","profile_id":"capture-profile"}','{"api_key":1}')`, custodian, owned.MerchantID.UUID(), custodian.String(), g.account)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(ctx, `UPDATE billing.psps SET custodian_id=$1 WHERE id=$2`, custodian, psp)
	require.NoError(t, err)
	name, err := merchants.CustodianSecretName("hyperswitch", "test", g.account, "api_key")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, name, "capture-fixture-key")
	require.NoError(t, err)
	var account string
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT account_id FROM billing.psps WHERE id=$1`, psp).Scan(&account))
	name, err = merchants.PSPSecretName("nmi", "test", account, "security_key")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, name, "invoice-key")
	require.NoError(t, err)
	rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIDirectPostURL = "https://secure.nmi.com/api/transact.php"
	client := surface.Client(openrails.WithAPIKey(owned.APIKey))
	customer := openrails.CustomerID(uuid.MustParse(user.ID))
	setup, err := client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{Mode: "payment_method", IdempotencyKey: uuid.NewString(), Customer: openrails.CheckoutCustomerIdentity{ID: customer, VerifiedEmail: *user.Email, Username: "invoice"}, Payment: openrails.CheckoutPayment{PSPID: psp}})
	require.NoError(t, err)
	complete, err := client.ConfirmCheckoutSession(ctx, setup.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: customer, Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: custodian, SessionID: setup.Capture.SessionID, Token: g.complete(setup.Capture.SessionID)}}})
	require.NoError(t, err)
	require.NotNil(t, complete.PaymentMethodID)
	var financial int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1)+(SELECT count(*) FROM billing.invoices WHERE merchant_id=$1)+(SELECT count(*) FROM billing.ledger_accounts WHERE merchant_id=$1)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1)`, owned.MerchantID.UUID()).Scan(&financial))
	require.Zero(t, financial)
	var invoice uuid.UUID
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		payer := identity.CustomerID(customer.UUID())
		mode := money.BillingModeArrears
		if _, err := rt.MoneyService.UpsertAccountSettings(c, payer, "USD", money.AccountSettingsInput{BillingMode: &mode}); err != nil {
			return err
		}
		if _, err := rt.MoneyService.AccrueOwed(c, payer, "USD", "client-invoice", uuid.NewString(), 5_000_000); err != nil {
			return err
		}
		issued, err := rt.MoneyService.FinalizeInvoice(c, payer, "USD", time.Now().Add(-time.Hour), time.Now())
		if err == nil {
			invoice = issued.ID
		}
		return err
	}))
	payerClient, err := openrails.NewRemote(surface.BaseURL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
	require.NoError(t, err)
	request := openrails.PayInvoiceNowRequest{InvoiceID: invoice, PaymentMethodID: *complete.PaymentMethodID, IdempotencyKey: uuid.NewString()}
	paid, err := payerClient.PayInvoiceNow(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "succeeded", paid.Operation.Status)
	require.Equal(t, "paid", paid.Invoice.Status)
	replay, err := payerClient.PayInvoiceNow(ctx, request)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, paid.Operation.ID, replay.Operation.ID)
	request.PaymentMethodID = openrails.PaymentMethodID(uuid.New())
	_, err = payerClient.PayInvoiceNow(ctx, request)
	require.ErrorIs(t, err, openrails.ErrConflict)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, forms, 1)
	for _, form := range forms {
		require.Equal(t, "5.00", form["amount"])
		require.Equal(t, "customer", form["initiated_by"])
		require.Equal(t, "stored", form["stored_credential_indicator"])
	}
	var settled, transfers int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id=$1 AND status='settled'`, invoice).Scan(&settled))
	require.Equal(t, 1, settled)
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, customer.UUID()).Scan(&transfers))
	require.Equal(t, 1, transfers)
}
