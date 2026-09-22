//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// The same public Go command drives the internal transport and real HTTP.
// Saved cards have no initial agreement; the real accepted sale establishes it.
func TestQualifiedOneTimeSaleClientAcrossEmbeddedAndHTTP(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	gateway := NewFakeNMIGateway(t)
	var vaultCreates atomic.Int64
	vaultEntered, vaultRelease := make(chan struct{}, 1), make(chan struct{})
	defer close(vaultRelease)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/customers") {
			gateway.serve(w, r)
			return
		}
		var body struct {
			Billing struct {
				PaymentDetails struct {
					Token string `json:"payment_token"`
				} `json:"payment_details"`
			} `json:"billing"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Billing.PaymentDetails.Token == "" {
			http.Error(w, "token missing", http.StatusBadRequest)
			return
		}
		id := vaultCreates.Add(1)
		vaultEntered <- struct{}{}
		select {
		case <-vaultRelease:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, `{"object":"customer","id":"token-vault-%d","billing":[{"id":"token-billing-%d","priority":1}]}`, id, id)
	}))
	t.Cleanup(vault.Close)
	configure := func(c *config.Config) { c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: vault.URL} }
	server := h.StartStandalone("USD", WithConfig(configure))
	owned := server.ProvisionOwnedMerchant("sale-proof-" + uuid.NewString()[:8])
	psp := h.ArmLoopbackNMI(server.App().Runtime, owned.MerchantID)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}}
	configure(cfg)
	rt, err := embed.New(ctx, embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	embedded, err := rt.Client(openrails.WithMerchantID(owned.MerchantID))
	require.NoError(t, err)
	remote := server.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	for name, client := range map[string]*openrails.Client{"embedded": embedded, "http": remote} {
		for _, token := range []bool{false, true} {
			mode := "saved"
			if token {
				mode = "token"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				customer, method := uuid.New(), uuid.New()
				_, err := client.EnsureCustomer(ctx, openrails.CustomerID(customer))
				require.NoError(t, err)
				product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "sale-" + uuid.NewString(), DisplayName: "Accepted purchase", EntitlementsSpec: map[string]*int{"sale_qualified_access": nil}})
				require.NoError(t, err)
				price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 5_000_000, Currency: "USD"})
				require.NoError(t, err)
				if !token {
					_, err = dbtest.Queries(h.sharedPool()).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{ID: method, MerchantID: owned.MerchantID.UUID(), CustomerID: customer, PspID: psp, Rail: "nmi", RailCustomerRef: "vault-" + method.String(), RailMethodRef: "billing-" + method.String()})
					require.NoError(t, err)
				}
				request := openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: openrails.CustomerID(customer), VerifiedEmail: "sale@example.test", Username: "sale-buyer"}, PriceID: sdkPriceID(t, price.ID), IdempotencyKey: uuid.NewString(), Payment: openrails.CheckoutPayment{Rail: "loopback", PaymentMethodID: openrails.PaymentMethodID(method)}}
				before := gateway.SaleAttempts()
				var result *openrails.CheckoutSession
				if token {
					request.Payment.PaymentMethodID = openrails.PaymentMethodID{}
					request.Payment.PaymentToken = "token-" + uuid.NewString()
					beforeVaults := vaultCreates.Load()
					type completion struct {
						result *openrails.CheckoutSession
						err    error
					}
					finished := make(chan completion, 1)
					go func() { result, err := client.CreateCheckoutSession(ctx, request); finished <- completion{result, err} }()
					<-vaultEntered
					_, duplicateErr := client.CreateCheckoutSession(ctx, request)
					vaultRelease <- struct{}{}
					winner := <-finished
					require.Error(t, duplicateErr, "the existing public checkout lease refuses a second token consumer")
					require.Equal(t, beforeVaults+1, vaultCreates.Load())
					result, err = winner.result, winner.err
					require.NoError(t, err)
					require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT id FROM billing.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, owned.MerchantID.UUID(), customer).Scan(&method))
				} else {
					result, err = client.CreateCheckoutSession(ctx, request)
				}
				require.NoError(t, err)
				require.Equal(t, "succeeded", result.Status)
				require.Equal(t, before+1, gateway.SaleAttempts())
				last := gateway.Sales()[len(gateway.Sales())-1]
				require.Equal(t, "5.00", last.Amount)
				require.Equal(t, "USD", last.Currency)
				var anchor string
				require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT stored_credential_unscheduled_ref FROM billing.payment_methods WHERE merchant_id=$1 AND id=$2`, owned.MerchantID.UUID(), method).Scan(&anchor))
				require.Equal(t, last.TransactionID, anchor)
				archived := true
				_, err = client.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{Archived: &archived, SkipRailSync: true})
				require.NoError(t, err)
				replay, err := client.CreateCheckoutSession(ctx, request)
				require.NoError(t, err)
				require.Equal(t, result.ID, replay.ID)
				require.Equal(t, result.PaymentID, replay.PaymentID)
				require.Equal(t, before+1, gateway.SaleAttempts(), "replay ignores changed catalog and never charges again")
				changed := request
				changed.Customer.ID = openrails.CustomerID(uuid.New())
				_, err = client.CreateCheckoutSession(ctx, changed)
				require.Error(t, err)
				require.Equal(t, before+1, gateway.SaleAttempts())
				var payments, operations int
				require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND customer_id=$2 AND status='completed'`, owned.MerchantID.UUID(), customer).Scan(&payments))
				require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE merchant_id=$1 AND intent_type='nmi_sale' AND payload->>'user_id'=$2 AND status='succeeded'`, owned.MerchantID.UUID(), customer.String()).Scan(&operations))
				require.Equal(t, 1, payments)
				require.Equal(t, 1, operations)
			})
		}
	}
}
