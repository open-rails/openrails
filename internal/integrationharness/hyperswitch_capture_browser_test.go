//go:build integration && browser && hyperswitch

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	embedauth "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/operator"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Selected explicitly against the pinned local vendor, never silently skipped.
// Its card entry happens in the actual pinned vendor iframe, not this Go server.
func TestHyperSwitchActualBrowserInvoice(t *testing.T) {
	type recovery struct {
		DB              *config.DBConfig
		Redis           *config.RedisConfig
		HyperSwitch     *config.HyperSwitchConfig
		ProviderSandbox *config.ProviderSandboxConfig
		Encryption      *config.EncryptionConfig
		MerchantID      merchant.ID
		OperationID     uuid.UUID
	}
	if path := os.Getenv("OPENRAILS_HS_INVOICE_RECOVERY"); path != "" {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var input recovery
		require.NoError(t, json.Unmarshal(raw, &input))
		restored, err := embed.New(t.Context(), embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: input.DB, Redis: input.Redis, HyperSwitch: input.HyperSwitch, ProviderSandbox: input.ProviderSandbox, Encryption: input.Encryption}, River: embed.RiverManagedByOpenRails()})
		require.NoError(t, err)
		defer restored.Close(context.Background())
		runtime := app.HostGraph(restored).Runtime
		require.NoError(t, runtime.DB.RunInMerchantConn(merchant.WithID(t.Context(), input.MerchantID), func(c context.Context) error {
			operation, err := runtime.IntentRunner().VerifyByID(c, input.OperationID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusSucceeded, operation.Status)
			return err
		}))
		return
	}
	path := os.Getenv("OPENRAILS_HYPERSWITCH_FIXTURE")
	require.NotEmpty(t, path, "selecting this qualification requires the owned local HyperSwitch fixture")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var vendor struct {
		APIBaseURL          string `json:"api_base_url"`
		SDKURL              string `json:"sdk_url"`
		MerchantID          string `json:"merchant_id"`
		ProfileID           string `json:"profile_id"`
		PublicAPIKey        string `json:"public_api_key"`
		APIKey              string `json:"api_key"`
		NMIReadBase         string `json:"nmi_read_base_url"`
		NMIProxyDestination string `json:"nmi_proxy_destination"`
		NMIKey              string `json:"nmi_security_key"`
		RouterContainer     string `json:"router_container"`
	}
	require.NoError(t, json.Unmarshal(raw, &vendor))
	ctx := t.Context()
	type observation struct {
		Order, Transaction, Amount, Currency, Initiator, Indicator, Anchor, BillingMethod string
		CardMatched, Approved                                                             bool
	}
	observations := func() []observation {
		response, err := http.Get(vendor.NMIReadBase + "/observations")
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var rows []observation
		require.NoError(t, json.NewDecoder(response.Body).Decode(&rows))
		return rows
	}
	var before int
	before = len(observations())

	h := New(t, ctx)
	billingClock := NewSettableClock(nil)
	var delegated billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithClock(billingClock), WithConfig(func(c *config.Config) {

		c.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: vendor.APIBaseURL, SDKURL: vendor.SDKURL}
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
		{
			require.NotEmpty(t, vendor.NMIReadBase)
			require.NotEmpty(t, vendor.NMIProxyDestination)
			require.NotEmpty(t, vendor.NMIKey)
			c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: vendor.NMIReadBase}
		}
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			if delegated == nil {
				return nil, billingauth.ErrUnauthenticated
			}
			principal, err := delegated.AuthenticateDelegated(ctx, r)
			// A trusted host may omit interaction classification for nonfinancial
			// capture. Downgrade only; the real engine CIT retains AuthKit's class.
			if err == nil && principal != nil && r.Header.Get("X-Qualification-Unknown-Class") == "true" {
				principal.CredentialClass = billingauth.CredentialClassUnknown
			}
			return principal, err
		})
	})
	owned := surface.ProvisionOwnedMerchant("actual-capture-" + uuid.NewString()[:8])
	core := operator.Get(surface.App()).Core()
	auth := operator.Get(surface.App()).AuthService()
	delegated, err = embedauth.NewDelegatedAuthenticator(auth.Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	user, err := core.CreateUser(ctx, "capture-"+uuid.NewString()+"@example.test", "capture"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.NoError(t, core.MarkEmailVerified(ctx, user.ID))
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "nmi")
	custodian := uuid.New()
	settings, _ := json.Marshal(map[string]string{"public_api_key": vendor.PublicAPIKey, "profile_id": vendor.ProfileID})
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,$5,'{"api_key":1}')`, custodian, owned.MerchantID.UUID(), custodian.String(), vendor.MerchantID, settings)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(ctx, `UPDATE billing.psps SET custodian_id=$1 WHERE merchant_id=$2 AND id=$3`, custodian, owned.MerchantID.UUID(), psp)
	require.NoError(t, err)
	name, err := merchants.CustodianSecretName("hyperswitch", "test", vendor.MerchantID, "api_key")
	require.NoError(t, err)
	_, err = surface.App().Runtime.Merchants.Secrets().Put(ctx, owned.MerchantID, name, vendor.APIKey)
	require.NoError(t, err)
	rt := surface.App().Runtime
	ownerCtx := merchant.WithID(ctx, owned.MerchantID)
	recoverOperation := func(t *testing.T, operationID uuid.UUID) {
		private := filepath.Join(t.TempDir(), "recovery.json")
		input, err := json.Marshal(recovery{DB: rt.Config.DB, Redis: rt.Config.Redis, HyperSwitch: rt.Config.HyperSwitch, ProviderSandbox: rt.Config.ProviderSandbox, Encryption: rt.Config.Encryption, MerchantID: owned.MerchantID, OperationID: operationID})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(private, input, 0600))
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHyperSwitchActualBrowserInvoice$", "-test.v")
		child.Env = append(os.Environ(), "OPENRAILS_HS_INVOICE_RECOVERY="+private)
		output, err := child.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	payer := identity.CustomerID(uuid.MustParse(user.ID))
	{
		// The same fake PSP is reached from two isolated network namespaces:
		// the vendor's fixed internal write route and the host's read-only edge.
		// This existing test endpoint seam does not add a production URL option.
		rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIDirectPostURL = vendor.NMIProxyDestination
		var account string
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT account_id FROM billing.psps WHERE id=$1`, psp).Scan(&account))
		key, err := merchants.PSPSecretName("nmi", "test", account, "security_key")
		require.NoError(t, err)
		_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, key, vendor.NMIKey)
		require.NoError(t, err)
	}
	sdkURL, err := url.Parse(vendor.SDKURL)
	require.NoError(t, err)
	sdkOrigin := sdkURL.Scheme + "://" + sdkURL.Host
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/membership-recover" {
			var input struct {
				OperationID uuid.UUID `json:"operation_id"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			var financial int
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1)`, owned.MerchantID.UUID()).Scan(&financial))
			require.Zero(t, financial, "unknown payment grants no membership before exact receipt")
			require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
				operation, err := rt.IntentRunner().VerifyByID(c, input.OperationID)
				require.NoError(t, err)
				require.Equal(t, intents.StatusSucceeded, operation.Status)
				return err
			}))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/membership-expired" {
			var input struct {
				SessionID openrails.CheckoutSessionID `json:"session_id"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			_, err := h.sharedPool().Exec(ctx, `UPDATE billing.checkout_sessions SET status='expired',expires_at=$3 WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), input.SessionID.UUID(), billingClock.Now().Add(-time.Hour))
			require.NoError(t, err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/membership-quoted" {
			var input struct {
				PriceID   openrails.PriceID           `json:"price_id"`
				SessionID openrails.CheckoutSessionID `json:"session_id"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
			_, denied := owner.ConfirmCheckoutSession(ctx, input.SessionID, openrails.ConfirmCheckoutSessionRequest{CustomerID: openrails.CustomerID(uuid.MustParse(user.ID)), Payment: openrails.ConfirmPayment{Rail: "nmi"}})
			require.ErrorIs(t, denied, openrails.ErrDenied, "merchant authority and body customer ID cannot accept recurring agreement")
			var financial int
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1)`, owned.MerchantID.UUID()).Scan(&financial))
			require.Zero(t, financial, "priced quote is not payment or membership")
			_, err := h.sharedPool().Exec(ctx, `UPDATE billing.prices SET amount=1000000,access_duration_hours=24 WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), input.PriceID.UUID())
			require.NoError(t, err)
			_, err = h.sharedPool().Exec(ctx, `UPDATE billing.products SET entitlements_spec='{"changed_after_quote":null}' WHERE merchant_id=$1 AND id=(SELECT product_id FROM billing.prices WHERE merchant_id=$1 AND id=$2)`, owned.MerchantID.UUID(), input.PriceID.UUID())
			require.NoError(t, err)
			control, err := http.Post(vendor.NMIReadBase+"/control", "application/json", bytes.NewBufferString(`{"Next":"lost"}`))
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, control.StatusCode)
			_ = control.Body.Close()
			require.NoError(t, json.NewEncoder(w).Encode(map[string]int{"financial": financial, "provider_calls": len(observations())}))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/invoice-fixture" {
			var invoiceID uuid.UUID
			// Called by the test driver only after Save card completed.
			// Saving either volatile or persistent capture creates no money rows.
			var financial int
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1)+(SELECT count(*) FROM billing.invoices WHERE merchant_id=$1)+(SELECT count(*) FROM billing.ledger_accounts WHERE merchant_id=$1)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1)`, owned.MerchantID.UUID()).Scan(&financial))
			require.Zero(t, financial)
			require.Len(t, observations(), before)

			require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
				mode := money.BillingModeArrears
				if _, err := rt.MoneyService.UpsertAccountSettings(c, payer, "USD", money.AccountSettingsInput{BillingMode: &mode}); err != nil {
					return err
				}
				if _, err := rt.MoneyService.AccrueOwed(c, payer, "USD", "actual-vendor-invoice", uuid.NewString(), 5_000_000); err != nil {
					return err
				}
				issued, err := rt.MoneyService.FinalizeInvoice(c, payer, "USD", time.Now().Add(-time.Hour), time.Now())
				if err == nil {
					invoiceID = issued.ID
				}
				return err
			}))
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"invoice_id": invoiceID.String()}))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' "+sdkOrigin+"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src "+surface.BaseURL+" "+vendor.APIBaseURL+"; frame-src "+sdkOrigin+"; base-uri 'none'; object-src 'none'; form-action 'none'")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Local vendor capture qualification</h1><div id="number"></div><div id="expiry"></div><div id="cvc"></div><button id="save">Save card</button></body></html>`))
	}))
	t.Cleanup(page.Close)

	script, err := filepath.Abs("../../tests/browser/hyperswitch-capture.mjs")
	require.NoError(t, err)
	reader, err := hyperswitch.New(hyperswitch.Config{BaseURL: vendor.APIBaseURL, MerchantID: vendor.MerchantID, ProfileID: vendor.ProfileID, APIKey: hyperswitch.Secret(vendor.APIKey), ReadOnly: true})
	require.NoError(t, err)
	protected := []string{vendor.APIKey, "4111111111111111"}
	stores := []bool{false, true}
	protected = append(protected, vendor.NMIKey)
	var savedMethod openrails.PaymentMethodID
	for _, store := range stores {
		browserParams := map[string]string{"page": page.URL, "vendor_api": vendor.APIBaseURL, "vendor_sdk": vendor.SDKURL, "api": surface.BaseURL, "token": token, "psp_id": psp.String(), "email": "capture-browser@example.test", "name": "Capture Browser", "store": strconv.FormatBool(store)}
		if store {
			browserParams["invoice_fixture"] = page.URL + "/invoice-fixture"
		}
		input, _ := json.Marshal(browserParams)
		command := exec.CommandContext(ctx, "node", script)
		secretProof := filepath.Join(t.TempDir(), "browser-secrets.json")
		command.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(input), "OPENRAILS_BROWSER_PRIVATE="+secretProof)
		output, err := command.CombinedOutput()
		t.Log(string(output))
		require.NoError(t, err)
		secrets, err := os.ReadFile(secretProof)
		require.NoError(t, err)
		var values []string
		require.NoError(t, json.Unmarshal(secrets, &values))
		protected = append(protected, values...)
		var proof struct {
			InvoiceKey     string                         `json:"invoiceKey"`
			VendorSession  string                         `json:"vendorSession"`
			VendorCustomer string                         `json:"vendorCustomer"`
			Method         openrails.PaymentMethodID      `json:"method"`
			InvoicePay     *openrails.InvoicePayNowResult `json:"invoicePay"`
		}
		require.NoError(t, json.Unmarshal(output, &proof))
		session, err := reader.GetSession(ctx, proof.VendorSession, proof.VendorCustomer)
		require.NoError(t, err)
		require.Len(t, session.AssociatedMethods, 1)
		method, err := reader.GetMethod(ctx, session.AssociatedMethods[0].Token.Data, proof.VendorCustomer)
		if store {
			require.NoError(t, err)
			require.Equal(t, "persistent", method.StorageType)
		} else {
			require.ErrorIs(t, err, hyperswitch.ErrBinding)
			require.Equal(t, "volatile", method.StorageType)
		}
		t.Logf("Explicit storage consent=%t; vendor readback storage_type=%s", store, method.StorageType)
		if store {
			savedMethod = proof.Method
			require.NotNil(t, proof.InvoicePay)
			invoiceID := proof.InvoicePay.Invoice.ID
			require.Equal(t, "succeeded", proof.InvoicePay.Operation.Status)
			require.Equal(t, "paid", proof.InvoicePay.Invoice.Status)
			client, err := openrails.NewRemote(surface.BaseURL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
			require.NoError(t, err)
			read, err := client.GetMyInvoice(ctx, invoiceID)
			require.NoError(t, err)
			require.Equal(t, "paid", read.Status)
			_, err = client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{InvoiceID: invoiceID, PaymentMethodID: openrails.PaymentMethodID(uuid.New()), IdempotencyKey: proof.InvoiceKey})
			require.ErrorIs(t, err, openrails.ErrConflict, "changing the method under the accepted key refuses before a new send")
			var anchor, recurring string
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_unscheduled_ref,stored_credential_recurring_ref FROM billing.payment_methods WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), uuid.UUID(proof.Method)).Scan(&anchor, &recurring))
			require.NotEmpty(t, anchor)
			require.Empty(t, recurring)
			require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
				if err := rt.MoneyService.SetInvoiceCollectionPaymentMethod(c, payer, "JPY", uuid.UUID(proof.Method)); err != nil {
					return err
				}
				if _, err := rt.MoneyService.AccrueOwed(c, payer, "JPY", "actual-vendor-mit", uuid.NewString(), 30_001); err != nil {
					return err
				}
				if _, err := rt.MoneyService.FinalizeInvoice(c, payer, "JPY", time.Now().Add(-time.Minute), time.Now().Add(time.Hour)); err != nil {
					return err
				}
				n, err := rt.MoneyService.ChargeOutstanding(c, rt.IntentRunner(), 0)
				require.Equal(t, 1, n)
				return err
			}))
			rows := observations()[before:]
			require.Len(t, rows, 2)
			require.Equal(t, "5.00", rows[0].Amount)
			require.Equal(t, "USD", rows[0].Currency)
			require.Equal(t, "customer", rows[0].Initiator)
			require.Equal(t, "stored", rows[0].Indicator)
			require.Empty(t, rows[0].Anchor)
			require.Equal(t, anchor, rows[0].Transaction)
			require.Equal(t, "4.00", rows[1].Amount)
			require.Equal(t, "JPY", rows[1].Currency)
			require.Equal(t, "merchant", rows[1].Initiator)
			require.Equal(t, "used", rows[1].Indicator)
			require.Equal(t, anchor, rows[1].Anchor)
			for _, row := range rows {
				require.True(t, row.CardMatched)
				require.True(t, row.Approved)
			}
			for _, outcome := range []string{"lost", "declined"} {
				t.Run(outcome, func(t *testing.T) {
					baseline := len(observations())
					response, err := http.Post(vendor.NMIReadBase+"/control", "application/json", bytes.NewBufferString(`{"Next":"`+outcome+`"}`))
					require.NoError(t, err)
					response.Body.Close()
					require.Equal(t, http.StatusNoContent, response.StatusCode)
					var next uuid.UUID
					require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
						if _, err := rt.MoneyService.AccrueOwed(c, payer, "USD", "actual-vendor-"+outcome, uuid.NewString(), 5_000_000); err != nil {
							return err
						}
						issued, err := rt.MoneyService.FinalizeInvoice(c, payer, "USD", time.Now().Add(-time.Second), time.Now().Add(time.Hour))
						if err == nil {
							next = issued.ID
						}
						return err
					}))
					request := openrails.PayInvoiceNowRequest{InvoiceID: next, PaymentMethodID: proof.Method, IdempotencyKey: uuid.NewString()}
					result, err := client.PayInvoiceNow(ctx, request)
					if outcome == "lost" {
						require.NoError(t, err)
						require.Equal(t, "unknown_needs_verify", result.Operation.Status)
						// Re-exec the test binary: a new process and embedded runtime use
						// only committed operation data and configured read credentials.
						recoverOperation(t, result.Operation.ID)

					} else {
						var refusal *openrails.StatusError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, http.StatusPaymentRequired, refusal.Status)
						require.Equal(t, "card_declined", refusal.Code)
					}
					replay, err := client.PayInvoiceNow(ctx, request)
					if outcome == "lost" {
						require.NoError(t, err)
						require.True(t, replay.Replayed)
						require.Equal(t, result.Operation.ID, replay.Operation.ID)
					} else {
						var refusal *openrails.StatusError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, http.StatusPaymentRequired, refusal.Status)
						require.Equal(t, "card_declined", refusal.Code)
					}
					received := observations()[baseline:]
					require.Len(t, received, 1, "uncertainty and replay never resubmit")
					require.Equal(t, anchor, received[0].Anchor)
					require.Equal(t, "used", received[0].Indicator)
					require.Equal(t, "customer", received[0].Initiator)
					require.Equal(t, "5.00", received[0].Amount)
					require.Equal(t, outcome != "declined", received[0].Approved)
					var settled, transfers int
					require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id=$1 AND status='settled'`, next).Scan(&settled))
					require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, payer.UUID()).Scan(&transfers))
					if outcome == "lost" {
						require.Equal(t, 1, settled)
						require.Equal(t, 3, transfers)
					} else {
						require.Zero(t, settled)
						require.Equal(t, 3, transfers)
					}
				})
			}
			t.Run("terminal archive restore", func(t *testing.T) {
				var archive bytes.Buffer
				require.Error(t, merchantarchive.Export(ctx, rt.DB, owned.MerchantID, &archive), "unfinished volatile setup refuses export")
				require.Empty(t, archive.Bytes())
				require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
					_, err := rt.DB.Gen(c).ExpireCheckoutSessions(c, gen.ExpireCheckoutSessionsParams{MerchantID: owned.MerchantID.UUID(), Now: time.Now().Add(time.Hour), RowLimit: 100})
					return err
				}))
				require.NoError(t, merchantarchive.Export(ctx, rt.DB, owned.MerchantID, &archive))
				adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
				schema := "actual_hs_invoice_" + uuid.NewString()[:8]
				dbtest.ApplyPostgresMigrations(t, adminDSN, appDSN, schema)
				target, err := db.NewDB(ctx, &config.DBConfig{URL: appDSN, Schema: schema})
				require.NoError(t, err)
				t.Cleanup(func() { _ = target.Close() })
				_, err = target.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,'actual-hs-invoice-destination')`, owned.MerchantID.UUID())
				require.NoError(t, err)
				_, err = merchantarchive.Restore(ctx, target, owned.MerchantID, bytes.NewReader(archive.Bytes()))
				require.NoError(t, err)
				require.NoError(t, target.RunInMerchantConn(ownerCtx, func(c context.Context) error {
					restored := money.NewMoneyService(target)
					invoice, err := restored.GetInvoiceByID(c, payer, invoiceID)
					require.NoError(t, err)
					require.Equal(t, "paid", invoice.Status)
					require.Zero(t, invoice.AmountDue)
					// No provider adapter exists in this restored runner.
					operation, err := (&intents.Runner{Store: intents.NewStore(target)}).ExecuteByID(c, proof.InvoicePay.Operation.ID)
					require.NoError(t, err)
					require.Equal(t, intents.StatusSucceeded, operation.Status)
					payload, err := intents.DecodeInvoiceCollectionPayload(operation)
					require.NoError(t, err)
					require.NotNil(t, payload.HyperSwitch)
					require.Equal(t, vendor.MerchantID, payload.HyperSwitch.AccountID)
					require.Equal(t, vendor.ProfileID, payload.HyperSwitch.ProfileID)
					require.Equal(t, vendor.APIBaseURL, payload.HyperSwitch.APIBaseURL)
					receipt, found, err := intents.LoadCollectedReceipt(operation)
					require.NoError(t, err)
					require.True(t, found)
					require.NoError(t, receipt.Validate(operation))
					var settled, transfers int
					require.NoError(t, target.Qx(c).QueryRow(c, `SELECT count(*) FROM openrails.invoice_payments WHERE invoice_id=$1 AND status='settled'`, invoiceID).Scan(&settled))
					require.NoError(t, target.Qx(c).QueryRow(c, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, payer.UUID()).Scan(&transfers))
					require.Equal(t, 1, settled)
					require.Equal(t, 3, transfers)
					return nil
				}))
				require.Len(t, observations()[before:], 4, "restored terminal replay is local")
			})
			t.Run("actual recurring transport only", func(t *testing.T) {
				// Transport qualification after actual browser capture; this is not
				// recurring consent, subscription enrollment or lifecycle completion.
				var method gen.OpenrailsPaymentMethod
				require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
					var err error
					method, err = rt.DB.Gen(c).GetPaymentMethodByID(c, gen.GetPaymentMethodByIDParams{MerchantID: owned.MerchantID.UUID(), ID: uuid.UUID(proof.Method)})
					return err
				}))
				binding := charge.HyperSwitchBinding{AccountID: vendor.MerchantID, ProfileID: vendor.ProfileID, APIBaseURL: vendor.APIBaseURL}
				var initial string
				for _, phase := range []string{"initial", "reuse", "merchant", "lost", "declined"} {
					baseline := len(observations())
					outcome := "approved"
					if phase == "lost" || phase == "declined" {
						outcome = phase
					}
					body, _ := json.Marshal(map[string]string{"next": outcome})
					control, err := http.Post(vendor.NMIReadBase+"/control", "application/json", bytes.NewReader(body))
					require.NoError(t, err)
					require.Equal(t, http.StatusNoContent, control.StatusCode)
					_ = control.Body.Close()
					posture := charge.InitialRecurring()
					if phase == "reuse" {
						posture = charge.RecurringReuse(initial)
					} else if phase != "initial" {
						posture = charge.RecurringMIT(initial)
					}
					request := charge.Request{Instrument: charge.Instrument{PaymentMethodID: method.ID, Rail: "nmi", CustomerRef: method.RailCustomerRef, MethodRef: method.RailMethodRef}, AmountMinor: 123, Currency: "USD", OrderRef: uuid.NewString(), Context: posture}
					if phase == "merchant" {
						request.Currency = "JPY"
						request.AmountMinor = 100
					}
					var result charge.Result
					var declined bool
					err = rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
						charger, err := money.PrepareHyperSwitchCharge(c, rt.CollectionResolver, method, binding)
						if err != nil {
							return err
						}
						var refusal *nmi.CustomerVaultError
						if phase == "initial" || phase == "reuse" {
							result, refusal, err = charger.ChargeInitialRecurring(c, request)
						} else {
							result, refusal, err = charger.ChargeRecurringMIT(c, request)
						}
						declined = refusal != nil
						if refusal != nil {
							require.Equal(t, 200, refusal.ResponseCode)
						}
						return err
					})
					if phase == "lost" {
						require.ErrorIs(t, err, hyperswitch.ErrUnknown)
						require.False(t, errors.Is(err, charge.ErrNotDispatched))
						require.False(t, declined)
					} else {
						require.NoError(t, err)
					}
					received := observations()[baseline:]
					require.Len(t, received, 1, "actual vendor never retries a possibly submitted recurring charge")
					row := received[0]
					require.Equal(t, request.OrderRef, row.Order)
					require.Equal(t, "recurring", row.BillingMethod)
					require.True(t, row.CardMatched)
					require.Equal(t, string(posture.Initiator), row.Initiator)
					require.Equal(t, posture.PriorRef, row.Anchor)
					amount := "1.23"
					if phase == "merchant" {
						amount = "100.00"
					}
					require.Equal(t, amount, row.Amount)
					require.Equal(t, request.Currency, row.Currency)
					if phase == "initial" {
						require.Equal(t, "stored", row.Indicator)
						require.Equal(t, row.Transaction, result.CapturedRef)
						initial = row.Transaction
					} else {
						require.Equal(t, "used", row.Indicator)
						require.Empty(t, result.CapturedRef)
					}
					if phase == "declined" {
						require.True(t, declined)
						require.True(t, result.Declined)
						require.False(t, row.Approved)
					} else {
						require.True(t, row.Approved)
						readReq, err := http.NewRequestWithContext(ctx, http.MethodGet, vendor.NMIReadBase+"/payments/"+row.Transaction, nil)
						require.NoError(t, err)
						readReq.Header.Set("Authorization", vendor.NMIKey)
						readResp, err := http.DefaultClient.Do(readReq)
						require.NoError(t, err)
						require.Equal(t, http.StatusOK, readResp.StatusCode)
						var receipt struct{ ID, Amount, Currency string }
						require.NoError(t, json.NewDecoder(readResp.Body).Decode(&receipt))
						_ = readResp.Body.Close()
						require.Equal(t, row.Transaction, receipt.ID)
						require.Equal(t, amount, receipt.Amount)
						require.Equal(t, request.Currency, receipt.Currency)
					}
				}
				calls, err := http.Get(vendor.NMIReadBase + "/schedule-calls")
				require.NoError(t, err)
				var count struct{ Count int }
				require.NoError(t, json.NewDecoder(calls.Body).Decode(&count))
				_ = calls.Body.Close()
				require.Zero(t, count.Count, "HS never invokes native schedule operations")
				var retained string
				require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, method.ID).Scan(&retained))
				require.Empty(t, retained, "transport result alone cannot establish persisted recurring authority")
			})
		}
	}
	router := vendor.RouterContainer
	if router == "" {
		router = "openrails-297-nmi-form-20260920-router-1"
	}
	routerLogs, err := exec.CommandContext(ctx, "docker", "logs", router).CombinedOutput()
	require.NoError(t, err)
	for i, value := range protected {
		require.NotEmpty(t, value)
		require.False(t, bytes.Contains(routerLogs, []byte(value)), "protected capture value %d appeared in vendor logs", i)
	}
	t.Log("Vendor INFO logs contain neither synthetic PAN, merchant API key, SDK authorizations nor native session token")
	var methods, financial int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, owned.MerchantID.UUID(), user.ID).Scan(&methods))
	require.Equal(t, 1, methods)
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND customer_id=$2)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1 AND customer_id=$2)`, owned.MerchantID.UUID(), user.ID).Scan(&financial))
	require.Zero(t, financial, "invoice collections do not create checkout payments or subscriptions")

	t.Run("verified browser initial membership", func(t *testing.T) {
		// Inject business time at the original acceptance, then advance to the
		// real paid boundary. Provider/River IO keeps its ordinary wall clock.
		acceptedAt := time.Now().UTC().Add(-720 * time.Hour).Truncate(time.Microsecond)
		clock := clockwork.NewFakeClockAt(acceptedAt)
		billingClock.Set(clock)
		defer billingClock.Set(nil)

		owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
		product, err := owner.CreateProduct(ctx, openrails.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Browser membership", EntitlementsSpec: map[string]*int{"browser_member": nil}})
		require.NoError(t, err)
		hours := 720
		price, err := owner.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
		require.NoError(t, err)
		beforeMembership := len(observations())
		params, _ := json.Marshal(map[string]string{"phase": "membership", "page": page.URL, "checkpoint": page.URL + "/membership-quoted", "expire": page.URL + "/membership-expired", "recover": page.URL + "/membership-recover", "before_provider_calls": strconv.Itoa(beforeMembership), "api": surface.BaseURL, "token": token, "psp_id": psp.String(), "price_id": price.ID.String(), "method": savedMethod.String()})
		command := exec.CommandContext(ctx, "node", script)
		command.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(params))
		output, err := command.CombinedOutput()
		t.Log(string(output))
		require.NoError(t, err)
		var proof struct {
			Membership struct {
				SubscriptionID openrails.SubscriptionID `json:"subscription_id"`
				PaymentID      openrails.PaymentID      `json:"payment_id"`
			} `json:"membership"`
		}
		require.NoError(t, json.Unmarshal(output, &proof))
		rows := observations()[beforeMembership:]
		require.Len(t, rows, 1, "confirmation and replay submit exactly one recurring CIT")
		require.Equal(t, "recurring", rows[0].BillingMethod)
		require.Equal(t, "customer", rows[0].Initiator)
		require.Equal(t, "stored", rows[0].Indicator)
		require.Equal(t, "9.99", rows[0].Amount)
		var policy, providerID, anchor, status string
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT collection_policy,rail_subscription_id,status FROM billing.subscriptions WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&policy, &providerID, &status))
		require.Equal(t, "engine", policy)
		require.Empty(t, providerID)
		require.Equal(t, "active", status)
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), savedMethod.UUID()).Scan(&anchor))
		require.Equal(t, rows[0].Transaction, anchor)
		var amount int64
		var paidStatus, transaction string
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT amount,status,transaction_id FROM billing.payments WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), proof.Membership.PaymentID.UUID()).Scan(&amount, &paidStatus, &transaction))
		require.EqualValues(t, 9_990_000, amount, "post-quote catalog edit cannot change accepted price")
		require.Equal(t, "completed", paidStatus)
		require.Equal(t, rows[0].Transaction, transaction)
		access, err := owner.HasEntitlement(ctx, openrails.CustomerID(uuid.MustParse(user.ID)), "browser_member", time.Now())
		require.NoError(t, err)
		require.True(t, access, "shared membership commit grants the accepted entitlement")
		var initialStart, initialEnd time.Time
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT current_period_starts_at,current_period_ends_at FROM billing.subscriptions WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&initialStart, &initialEnd))
		require.Equal(t, 720*time.Hour, initialEnd.Sub(initialStart))
		require.True(t, initialStart.Equal(acceptedAt))
		clock.Advance(initialEnd.Sub(clock.Now()))

		// Use Runtime's production worker registry and one ordinary River
		// client. A dedicated owned queue isolates these explicit ticks from
		// unrelated periodic maintenance without replacing any worker.
		const queue = "engine_browser"
		require.NoError(t, rt.AddRiverConfigurer(func(_ context.Context, c *river.Config) error {
			c.Queues = map[string]river.QueueConfig{queue: {MaxWorkers: 1}}
			return nil
		}))
		require.NoError(t, rt.InitRiver(ctx))
		require.NoError(t, rt.RiverClient.Start(ctx))
		defer func() { require.NoError(t, rt.RiverClient.Stop(context.WithoutCancel(ctx))) }()
		runJob := func(args river.JobArgs) {
			job, err := rt.RiverClient.Insert(ctx, args, &river.InsertOpts{Queue: queue, MaxAttempts: 1})
			require.NoError(t, err)
			if args.Kind() == intents.OperationJobKind {
				require.Eventually(t, func() bool {
					row, err := rt.RiverClient.JobGet(ctx, job.Job.ID)
					return err == nil && row.Attempt > 0 && (string(row.State) == "scheduled" || string(row.State) == "completed")
				}, 20*time.Second, 20*time.Millisecond, "operation worker must retain unknown work for verification")
				return
			}
			require.Eventually(t, func() bool {
				row, err := rt.RiverClient.JobGet(ctx, job.Job.ID)
				return err == nil && (string(row.State) == "completed" || string(row.State) == "discarded")
			}, 20*time.Second, 20*time.Millisecond, "registered worker must finish")
			row, err := rt.RiverClient.JobGet(ctx, job.Job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", string(row.State), "%+v", row.Errors)
		}
		runJob(riverjobs.DunningArgs{})
		var renewal gen.OpenrailsRailIntent
		require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
			var err error
			renewal, err = rt.DB.Gen(c).GetUnresolvedSubscriptionCollection(c, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: owned.MerchantID.UUID(), SubscriptionID: proof.Membership.SubscriptionID.UUID()})
			return err
		}))
		terms, err := subscriptions.DecodeSubscriptionCollectionPayload(renewal)
		require.NoError(t, err)
		require.EqualValues(t, 9_990_000, terms.Renewal.Amount)
		require.Equal(t, "USD", terms.Renewal.Currency)
		require.Equal(t, 720*time.Hour, terms.Renewal.PeriodEnd.Sub(terms.Renewal.PeriodStart))
		require.Equal(t, map[string]*int{"browser_member": nil}, terms.Renewal.Entitlements)
		require.True(t, terms.PreviousPeriodEnd.Equal(initialEnd))
		require.True(t, terms.Renewal.PeriodStart.Equal(initialEnd))
		var liveAmount int64
		var liveHours int
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT amount,access_duration_hours FROM billing.prices WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), price.ID.UUID()).Scan(&liveAmount, &liveHours))
		require.EqualValues(t, 1_000_000, liveAmount)
		require.Equal(t, 24, liveHours, "qualification must not reset the mutated catalog")

		beforeMIT := len(observations())
		control, err := http.Post(vendor.NMIReadBase+"/control", "application/json", bytes.NewBufferString(`{"Next":"lost"}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, control.StatusCode)
		control.Body.Close()
		runJob(intents.OperationArgs{MerchantID: owned.MerchantID.UUID(), IntentID: renewal.ID})
		loadRenewal := func() gen.OpenrailsRailIntent {
			var row gen.OpenrailsRailIntent
			require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error {
				var err error
				row, err = intents.NewStore(rt.DB).Get(c, renewal.ID)
				return err
			}))
			return row
		}
		uncertain := loadRenewal()
		require.Equal(t, intents.StatusUnknownNeedsVerify, uncertain.Status)
		require.JSONEq(t, string(renewal.Payload), string(uncertain.Payload))
		var renewalPayments int
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND subscription_id=$2 AND attempt_kind='renewal'`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&renewalPayments))
		require.Zero(t, renewalPayments, "lost reply must not manufacture payment/access")
		runJob(riverjobs.DunningArgs{})
		require.Equal(t, uncertain.ID, loadRenewal().ID)
		require.NoError(t, rt.RiverClient.Stop(context.WithoutCancel(ctx)))
		recoverOperation(t, renewal.ID)
		completed := loadRenewal()
		require.Equal(t, intents.StatusSucceeded, completed.Status)
		require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(completed))
		received := observations()[beforeMIT:]
		require.Len(t, received, 1, "lost MIT plus process restart must keep one POST")
		require.Equal(t, "merchant", received[0].Initiator)
		require.Equal(t, "recurring", received[0].BillingMethod)
		require.Equal(t, "used", received[0].Indicator)
		require.Equal(t, anchor, received[0].Anchor)
		require.Equal(t, "9.99", received[0].Amount)
		require.Equal(t, "USD", received[0].Currency)
		require.True(t, received[0].Approved)
		require.True(t, received[0].CardMatched)
		var renewedStart, renewedEnd time.Time
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT current_period_starts_at,current_period_ends_at FROM billing.subscriptions WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&renewedStart, &renewedEnd))
		require.True(t, renewedStart.Equal(terms.Renewal.PeriodStart))
		require.True(t, renewedEnd.Equal(terms.Renewal.PeriodEnd))
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT amount,status,transaction_id FROM billing.payments WHERE merchant_id=$1 AND subscription_id=$2 AND attempt_kind='renewal'`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&amount, &paidStatus, &transaction))
		require.EqualValues(t, 9_990_000, amount)
		require.Equal(t, "completed", paidStatus)
		require.Equal(t, received[0].Transaction, transaction)
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND subscription_id=$2 AND attempt_kind='renewal'`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&renewalPayments))
		require.Equal(t, 1, renewalPayments)
		var settlementEvents, paidGrants int
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.host_outbox WHERE merchant_id=$1 AND payment_id IN (SELECT id FROM billing.payments WHERE merchant_id=$1 AND subscription_id=$2 AND attempt_kind='renewal')`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID()).Scan(&settlementEvents))
		require.Equal(t, 1, settlementEvents)
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.grants WHERE merchant_id=$1 AND source_type='subscription' AND source_id=$2 AND event='grant' AND starts_at=$3 AND ends_at=$4`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID().String(), renewedStart, renewedEnd).Scan(&paidGrants))
		require.Equal(t, 1, paidGrants, "one accepted benefit window is committed with the payment")

		var grantsBefore, grantsAfter int
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.grants WHERE merchant_id=$1 AND source_id=$2`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID().String()).Scan(&grantsBefore))
		require.NoError(t, rt.DB.RunInMerchantConn(ownerCtx, func(c context.Context) error { _, err := rt.IntentRunner().VerifyByID(c, renewal.ID); return err }))
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.grants WHERE merchant_id=$1 AND source_id=$2`, owned.MerchantID.UUID(), proof.Membership.SubscriptionID.UUID().String()).Scan(&grantsAfter))
		require.Equal(t, grantsBefore, grantsAfter)
		require.Len(t, observations()[beforeMIT:], 1)
		changed, err := owner.HasEntitlement(ctx, openrails.CustomerID(uuid.MustParse(user.ID)), "changed_after_quote", renewedStart.Add(time.Hour))
		require.NoError(t, err)
		require.False(t, changed)
		response, err := http.Get(vendor.NMIReadBase + "/schedule-calls")
		require.NoError(t, err)
		defer response.Body.Close()
		var scheduleCalls struct{ Count int }
		require.NoError(t, json.NewDecoder(response.Body).Decode(&scheduleCalls))
		require.Zero(t, scheduleCalls.Count, "engine enrollment never creates a native schedule")
	})
}
