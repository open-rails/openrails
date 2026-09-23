//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	embedauth "github.com/open-rails/openrails/internal/hostauth"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Ordinary deployment coverage uses loopback custody and PSP boundaries. The
// explicit browser qualification separately proves real vendor interpolation.
func TestHyperSwitchInvoiceClientWorkflow(t *testing.T) {
	t.Run("pending", func(t *testing.T) { testHyperSwitchInvoiceDeletionWorkflow(t, false) })
	t.Run("completed", func(t *testing.T) { testHyperSwitchInvoiceDeletionWorkflow(t, true) })
}
func testHyperSwitchInvoiceDeletionWorkflow(t *testing.T, deleteCompleted bool) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCaptureFixture(t)
	original := g.server.Config.Handler
	var mu sync.Mutex
	forms := map[string]map[string]string{}
	blockReceipt, lostDelete := true, !deleteCompleted
	expectedStatus, expectedAliases, expectedDeletes := http.StatusAccepted, 1, 2
	if deleteCompleted {
		expectedStatus, expectedAliases, expectedDeletes = http.StatusNoContent, 0, 1
	}
	deletes := 0
	nativeVault := "native-vault-" + uuid.NewString()
	nativeDeleted := false
	nativeDeletes := 0
	g.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/nmi/customers":
			require.Equal(t, "native-delete-key", r.Header.Get("Authorization"))
			if nativeDeleted {
				fmt.Fprint(w, `{"customers":[],"has_more":false}`)
				return
			}
			require.Equal(t, nativeVault, r.URL.Query().Get("id"))
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"customers": []any{map[string]any{"object": "customer", "id": nativeVault, "billing": []any{map[string]any{"id": "native-billing", "priority": 1}}}}, "has_more": false}))
		case r.Method == http.MethodDelete && r.URL.Path == "/nmi/customers/"+nativeVault:
			require.Equal(t, "native-delete-key", r.Header.Get("Authorization"))
			nativeDeletes++
			nativeDeleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v2/payment-methods/"):
			require.Equal(t, "api-key=capture-fixture-key", r.Header.Get("Authorization"))
			deletes++
			if lostDelete {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"id": strings.TrimPrefix(r.URL.Path, "/v2/payment-methods/")}))
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
			if blockReceipt {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			require.Equal(t, "invoice-key", r.Header.Get("Authorization"))
			_, _ = fmt.Fprint(w, `{"id":"invoice_receipt","object":"transaction","response":"1","amount":"5.00","currency":"USD","actions":[{"id":"invoice_sale","type":"sale","amount":"5.00","success":true,"response":"1"}]}`)
		default:
			mu.Unlock()
			original.ServeHTTP(w, r)
			mu.Lock()
		}
	})
	var delegated billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.HyperSwitch = &config.HyperSwitchConfig{AllowLoopbackHTTP: true, APIBaseURL: g.server.URL, SDKURL: g.server.URL + "/sdk.js"}
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
	var maintenanceEnabled bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM billing.destructive_action_switch`).Scan(&maintenanceEnabled))
	require.False(t, maintenanceEnabled, "self-service deletion uses default maintenance settings")
	core := operator.Get(surface.App()).Core()
	auth := operator.Get(surface.App()).AuthService()
	delegated, err := embedauth.NewDelegatedAuthenticator(auth.Verifier(), owned.MerchantID.String())
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
	client := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	customer := openrails.CustomerID(uuid.MustParse(user.ID))
	setup, err := client.CreatePaymentMethodSession(ctx, openrails.CreatePaymentMethodSessionRequest{IdempotencyKey: uuid.NewString(), Customer: openrails.CheckoutCustomerIdentity{ID: customer.String(), VerifiedEmail: *user.Email, Username: "invoice"}, PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: psp.String()}})
	require.NoError(t, err)
	complete, err := client.ConfirmCheckoutSession(ctx, setup.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: customer.String(), Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: custodian, SessionID: setup.Capture.SessionID, Token: g.complete(setup.Capture.SessionID)}}})
	require.NoError(t, err)
	require.NotNil(t, complete.PaymentMethodID)
	paymentMethodID, err := openrails.ParsePaymentMethodID(*complete.PaymentMethodID)
	require.NoError(t, err)
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
	request := openrails.PayInvoiceNowRequest{InvoiceID: invoice, PaymentMethodID: paymentMethodID, IdempotencyKey: uuid.NewString()}
	deleteMethod := func(access string, selected ...openrails.PaymentMethodID) int {
		method := *complete.PaymentMethodID
		if len(selected) > 0 {
			method = selected[0].String()
		}
		wire, err := http.NewRequestWithContext(ctx, http.MethodDelete, surface.BaseURL+"/v1/me/payment-methods/"+method, nil)
		require.NoError(t, err)
		if access != "" {
			wire.Header.Set("Authorization", "Bearer "+access)
		}
		wire.Header.Set(merchant.BindingHeader, owned.MerchantID.String())
		response, err := http.DefaultClient.Do(wire)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode
	}
	require.Equal(t, http.StatusUnauthorized, deleteMethod(""))
	foreign, err := core.CreateUser(ctx, "foreign-"+uuid.NewString()+"@example.test", "foreign"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.NoError(t, core.MarkEmailVerified(ctx, foreign.ID))
	foreignToken, _, err := core.MintAccessToken(ctx, foreign.ID, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, deleteMethod(foreignToken))
	pending, err := payerClient.PayInvoiceNow(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "unknown_needs_verify", pending.Operation.Status, "accepted charge lacks exact receipt")
	require.Equal(t, http.StatusConflict, deleteMethod(token), "unresolved charge pins its saved method")
	var collection uuid.UUID
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT collection_intent_id FROM billing.invoices WHERE id=$1`, invoice).Scan(&collection))
	mu.Lock()
	blockReceipt = false
	require.Zero(t, deletes)
	mu.Unlock()
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		runner := rt.IntentRunner()
		runner.Clock = clockwork.NewFakeClockAt(time.Now().Add(time.Hour))
		row, err := runner.VerifyByID(c, collection)
		if err == nil {
			require.Equal(t, "succeeded", row.Status)
		}
		return err
	}))
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
	require.Len(t, forms, 1)
	for _, form := range forms {
		require.Equal(t, "5.00", form["amount"])
		require.Equal(t, "customer", form["initiated_by"])
		require.Equal(t, "stored", form["stored_credential_indicator"])
	}
	mu.Unlock()
	// A second PSP capture is already reading the same vendor card when
	// deletion wins admission. Its later attachment must join the handle fence.
	aliasPSP := uuid.New()
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,custodian_id) VALUES($1,$2,'nmi','test',$3,$4)`, aliasPSP, owned.MerchantID.UUID(), "capture-alias-"+aliasPSP.String(), custodian)
	require.NoError(t, err)
	aliasSetup, err := client.CreatePaymentMethodSession(ctx, openrails.CreatePaymentMethodSessionRequest{IdempotencyKey: uuid.NewString(), Customer: openrails.CheckoutCustomerIdentity{ID: customer.String(), VerifiedEmail: *user.Email, Username: "invoice"}, PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: aliasPSP.String()}})
	require.NoError(t, err)
	var sharedHandle string
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT rail_method_ref FROM billing.payment_methods WHERE id=$1`, paymentMethodID.UUID()).Scan(&sharedHandle))
	aliasToken := g.complete(aliasSetup.Capture.SessionID)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	g.mu.Lock()
	g.sessions[aliasSetup.Capture.SessionID].MethodID = sharedHandle
	g.afterMethodRead = func() { close(entered); <-release }
	g.mu.Unlock()
	finished := make(chan error, 1)
	go func() {
		_, err := client.ConfirmCheckoutSession(ctx, aliasSetup.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: customer.String(), Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: custodian, SessionID: aliasSetup.Capture.SessionID, Token: aliasToken}}})
		finished <- err
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("capture ended before provider-read barrier: %v", err)
	}
	require.Equal(t, expectedStatus, deleteMethod(token), "DELETE reports its actual durable result")
	release <- struct{}{}
	require.ErrorIs(t, <-finished, openrails.ErrConflict, "capture already in provider read cannot attach behind accepted deletion")
	g.mu.Lock()
	g.afterMethodRead = nil
	g.mu.Unlock()
	var aliasCount int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payment_methods WHERE merchant_id=$1 AND custodian_id=$2 AND rail_method_ref=$3`, owned.MerchantID.UUID(), custodian, sharedHandle).Scan(&aliasCount))
	require.Equal(t, expectedAliases, aliasCount)
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		_, err := rt.DB.Gen(c).ExpireCheckoutSessions(c, gen.ExpireCheckoutSessionsParams{MerchantID: owned.MerchantID.UUID(), Now: time.Now().Add(time.Hour), RowLimit: 100})
		return err
	}))

	var blockedInvoice uuid.UUID
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		payer := identity.CustomerID(customer.UUID())
		if _, err := rt.MoneyService.AccrueOwed(c, payer, "USD", "delete-first", uuid.NewString(), 2_000_000); err != nil {
			return err
		}
		invoice, err := rt.MoneyService.FinalizeInvoice(c, payer, "USD", time.Now().Add(-time.Second), time.Now().Add(time.Hour))
		if err == nil {
			blockedInvoice = invoice.ID
		}
		return err
	}))
	_, err = payerClient.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{InvoiceID: blockedInvoice, PaymentMethodID: paymentMethodID, IdempotencyKey: uuid.NewString()})
	require.Error(t, err, "accepted deletion prevents a new invoice charge")
	mu.Lock()
	require.Len(t, forms, 1, "delete-first ordering sends no new money POST")
	mu.Unlock()

	var deletion uuid.UUID
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT id FROM billing.rail_intents WHERE merchant_id=$1 AND intent_type='hyperswitch_method_delete' AND payload->>'payment_method_id'=$2`, owned.MerchantID.UUID(), paymentMethodID.UUID().String()).Scan(&deletion))
	mu.Lock()
	require.Equal(t, 1, deletes)
	lostDelete = false
	mu.Unlock()
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		runner := rt.IntentRunner()
		clock := clockwork.NewFakeClockAt(time.Now().Add(time.Hour))
		runner.Clock = clock
		_, err := runner.VerifyByID(c, deletion)
		if err != nil {
			return err
		}
		clock.Advance(time.Hour)
		row, err := runner.ExecuteByID(c, deletion)
		if err == nil {
			require.Equal(t, "succeeded", row.Status)
		}
		return err
	}))
	require.Equal(t, http.StatusNotFound, deleteMethod(token))
	replay, err = payerClient.PayInvoiceNow(ctx, request)
	require.Error(t, err, "the deliberately changed method remains a key conflict")
	request.PaymentMethodID = paymentMethodID
	replay, err = payerClient.PayInvoiceNow(ctx, request)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	mu.Lock()
	require.Len(t, forms, 1)
	require.Equal(t, expectedDeletes, deletes)
	mu.Unlock()
	// Native NMI uses the same authenticated self-service policy at defaults.
	nativePSP, nativeMethod := uuid.New(), uuid.New()
	nativeAccount := "native-delete-" + nativePSP.String()
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,'nmi','test',$3)`, nativePSP, owned.MerchantID.UUID(), nativeAccount)
	require.NoError(t, err)
	secretName, err := merchants.PSPSecretName("nmi", "test", nativeAccount, "security_key")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, secretName, "native-delete-key")
	require.NoError(t, err)
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
		return paymentmethods.NewPaymentMethodRepo(rt.DB).Create(c, &models.PaymentMethod{ID: nativeMethod, CustomerID: customer.UUID(), PspID: nativePSP, Rail: models.RailNMI, Custodian: models.CustodianPSP, RailCustomerRef: nativeVault, RailMethodRef: "native-billing"})
	}))
	rt.RailPaymentMethodService.NMIClients = &railresolve.NMIFactory{Config: rt.Config, Endpoints: railresolve.LoopbackNMIEndpoints(g.server.URL + "/nmi")}
	require.Equal(t, http.StatusNoContent, deleteMethod(token, openrails.PaymentMethodID(nativeMethod)))
	mu.Lock()
	require.True(t, nativeDeleted)
	require.Equal(t, 1, nativeDeletes)
	mu.Unlock()
	var settled, transfers int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id=$1 AND status='settled'`, invoice).Scan(&settled))
	require.Equal(t, 1, settled)
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, customer.UUID()).Scan(&transfers))
	require.Equal(t, 1, transfers)
	t.Run("deleted method archive stays deleted", func(t *testing.T) {
		_, err := client.EnsureCustomer(ctx, (openrails.CustomerID(uuid.MustParse(foreign.ID))).String())
		require.NoError(t, err)
		var original []byte
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT to_jsonb(i) FROM billing.rail_intents i WHERE id=$1`, deletion).Scan(&original))
		replaceRecord := func(raw []byte) {
			_, err := h.sharedPool().Exec(context.WithoutCancel(ctx), `UPDATE billing.rail_intents i SET payload=r.payload,result_evidence=r.result_evidence,actor=r.actor,idempotency_key=r.idempotency_key FROM jsonb_populate_record(NULL::billing.rail_intents,$1) r WHERE i.id=r.id`, raw)
			require.NoError(t, err)
		}
		for _, mode := range []string{"missing", "wrong payer", "forged completion"} {
			t.Run(mode, func(t *testing.T) {
				defer replaceRecord(original)
				var row map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(original, &row))
				if mode == "forged completion" {
					var evidence map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(row["result_evidence"], &evidence))
					evidence["physically_deleted"] = json.RawMessage("false")
					row["result_evidence"], err = json.Marshal(evidence)
					require.NoError(t, err)
				} else {
					var payload map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(row["payload"], &payload))
					if mode == "wrong payer" {
						payload["customer_id"], err = json.Marshal(foreign.ID)
						require.NoError(t, err)
						row["actor"], err = json.Marshal(foreign.ID)
						require.NoError(t, err)
					} else {
						other := uuid.NewString()
						payload["payment_method_id"], err = json.Marshal(other)
						require.NoError(t, err)
						row["idempotency_key"], err = json.Marshal(intents.TypeHyperSwitchMethodDelete + ":" + other)
						require.NoError(t, err)
					}
					row["payload"], err = json.Marshal(payload)
					require.NoError(t, err)
				}
				modified, err := json.Marshal(row)
				require.NoError(t, err)
				replaceRecord(modified)
				var invalid bytes.Buffer
				require.Error(t, merchantarchive.Export(ctx, rt.DB, owned.MerchantID, &invalid), "nullable invoice history requires exact completed deletion")
				require.Zero(t, invalid.Len())
			})
		}
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
			restored, err := intents.NewStore(rt.DB).Get(c, deletion)
			require.NoError(t, err)
			method, payer, err := intents.DeletedMethod(restored)
			require.NoError(t, err)
			require.Equal(t, paymentMethodID.UUID(), method)
			require.Equal(t, customer.UUID(), payer)
			return nil
		}))
		var artifact bytes.Buffer
		require.NoError(t, merchantarchive.Export(ctx, rt.DB, owned.MerchantID, &artifact))
		adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
		schema := "custody_delete_" + uuid.NewString()[:8]
		dbtest.ApplyPostgresMigrations(t, adminDSN, appDSN, schema)
		target, err := db.NewDB(ctx, &config.DBConfig{URL: appDSN, Schema: schema})
		require.NoError(t, err)
		t.Cleanup(func() { _ = target.Close() })
		_, err = target.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,'custody-delete-restore')`, owned.MerchantID.UUID())
		require.NoError(t, err)
		_, err = merchantarchive.Restore(ctx, target, owned.MerchantID, bytes.NewReader(artifact.Bytes()))
		require.NoError(t, err)
		var again bytes.Buffer
		require.NoError(t, merchantarchive.Export(ctx, target, owned.MerchantID, &again))
		require.Equal(t, artifact.String(), again.String())
		require.NoError(t, target.RunInMerchantConn(merchant.WithID(ctx, owned.MerchantID), func(c context.Context) error {
			var count int
			require.NoError(t, target.Qx(c).QueryRow(c, `SELECT count(*) FROM openrails.payment_methods WHERE id=$1`, paymentMethodID.UUID()).Scan(&count))
			require.Zero(t, count)
			operation, err := (&intents.Runner{Store: intents.NewStore(target)}).ExecuteByID(c, deletion)
			require.NoError(t, err)
			require.Equal(t, intents.StatusSucceeded, operation.Status)
			accepted, err := intents.DecodeHyperSwitchMethodDelete(operation)
			require.NoError(t, err)
			require.NoError(t, paymentmethods.NewPaymentMethodRepo(target).Create(c, &models.PaymentMethod{ID: accepted.PaymentMethodID, CustomerID: accepted.CustomerID, PspID: accepted.Instrument.PSPID, Rail: models.RailNMI, Custodian: models.CustodianHyperSwitch, CustodianID: accepted.Instrument.CustodianID, RailCustomerRef: accepted.Instrument.RailCustomerRef, RailMethodRef: accepted.Instrument.RailMethodRef}))
			return nil
		}))
		var corrupt bytes.Buffer
		require.Error(t, merchantarchive.Export(ctx, target, owned.MerchantID, &corrupt), "archive must refuse a resurrected deleted method")
		mu.Lock()
		require.Equal(t, expectedDeletes, deletes)
		require.Equal(t, 1, nativeDeletes)
		require.Len(t, forms, 1)
		mu.Unlock()
	})

}
