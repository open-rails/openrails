//go:build integration

package embed_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// fakeNMICheckoutGateway is a loopback NMI: vault creation succeeds, and every
// classic sale answers with the scripted response code.
type fakeNMICheckoutGateway struct {
	*httptest.Server
	saleResponseCode atomic.Value // string: NMI response_code for type=sale
	vaults           atomic.Int64
}

func newFakeNMICheckoutGateway(t *testing.T) *fakeNMICheckoutGateway {
	t.Helper()
	f := &fakeNMICheckoutGateway{}
	f.saleResponseCode.Store("100")
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/customers") {
			n := f.vaults.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"customer","id":"vault_%d","billing":[{"id":"bill_%d","priority":1}]}`, n, n)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("type") == "sale" {
			code := f.saleResponseCode.Load().(string)
			if code == "100" {
				fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn_%s&response_code=100", uuid.NewString()[:8])
				return
			}
			fmt.Fprintf(w, "response=2&responsetext=DECLINED&response_code=%s", code)
			return
		}
		fmt.Fprint(w, `<nm_response></nm_response>`)
	}))
	t.Cleanup(f.Close)
	return f
}

// refusalObservation is what a caller can branch on for one checkout attempt.
type refusalObservation struct {
	OK            bool
	Status        int
	Type          string
	Code          string
	DeclineReason any
	FailureCode   any
	Refused       bool
	CardDeclined  bool
	Stale         bool
	ProviderRej   bool
	Internal      bool
}

func observeRefusal(t *testing.T, label string, session *openrails.CheckoutSession, err error) refusalObservation {
	t.Helper()
	if err == nil {
		require.NotNil(t, session, label)
		return refusalObservation{OK: true, Code: session.Status}
	}
	var se *openrails.StatusError
	require.ErrorAs(t, err, &se, "%s: %v", label, err)
	return refusalObservation{
		Status: se.Status, Type: se.Type, Code: se.Code,
		DeclineReason: se.Metadata["decline_reason"], FailureCode: se.Metadata["failure_code"],
		Refused:      errors.Is(err, openrails.ErrPaymentRefused),
		CardDeclined: errors.Is(err, openrails.ErrCardDeclined),
		Stale:        errors.Is(err, openrails.ErrPaymentMethodStale),
		ProviderRej:  errors.Is(err, openrails.ErrPaymentProviderRejected),
		Internal:     errors.Is(err, openrails.ErrInternal),
	}
}

// TestCheckoutRefusalsAreCodedAcrossDeployments drives NMI one-off checkouts
// through the shared Client against a loopback gateway: an approved sale, an
// issuer decline, a gateway rejection and a saved payment method that no
// longer exists. Embedded and standalone must classify each identically, with
// the stable refusal codes rather than an opaque 500.
func TestCheckoutRefusalsAreCodedAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	gateway := newFakeNMICheckoutGateway(t)
	rails := config.PSPSet{"nmi": {Rail: models.RailNMI, AccountID: fmt.Sprintf("gw-%d", time.Now().UnixNano()), NMI: &config.NMIRailConfig{SecurityKey: "sk-refusal"}}}
	standalone := h.StartStandalone("USD", integrationharness.WithRails(rails))
	standalone.App().Runtime.CheckoutService.NMIEndpointOverride = gateway.URL
	standalone.App().Runtime.RailPaymentMethodService.NMIEndpointOverride = gateway.URL

	local, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close(context.Background())) })
	app.HostGraph(local).Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)
	app.HostGraph(local).Runtime.CheckoutService.NMIEndpointOverride = gateway.URL
	app.HostGraph(local).Runtime.RailPaymentMethodService.NMIEndpointOverride = gateway.URL
	inprocess, err := local.Client()
	require.NoError(t, err)

	mid := dbtest.TestMerchantID.UUID()
	productID, priceID := uuid.New(), uuid.New()
	_, err = h.Pool().Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Refusal fixture')`, productID, mid, "refusal-"+productID.String())
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,5000000,'USD',720,false)`, priceID, mid, productID, "refusal-"+priceID.String())
	require.NoError(t, err)

	type step struct {
		name         string
		saleResponse string
		payment      func(customer openrails.CustomerID) openrails.CheckoutPayment
	}
	token := func(openrails.CustomerID) openrails.CheckoutPayment {
		return openrails.CheckoutPayment{Rail: "nmi", PaymentToken: "tok_" + uuid.NewString()[:8], NameOnCard: "Test Buyer", Zip: "90210", Country: "US", LastFour: "4242", CardType: "visa", ExpiryDate: "1230"}
	}
	steps := []step{
		{name: "approved", saleResponse: "100", payment: token},
		{name: "insufficient_funds", saleResponse: "202", payment: token},
		{name: "gateway_rejected", saleResponse: "300", payment: token},
		{name: "stale_saved_card", saleResponse: "100", payment: func(openrails.CustomerID) openrails.CheckoutPayment {
			return openrails.CheckoutPayment{Rail: "nmi", PaymentMethodID: openrails.PaymentMethodID(uuid.New())}
		}},
	}

	observed := map[string]map[string]refusalObservation{}
	for name, client := range map[string]*openrails.Client{"embedded": inprocess, "standalone": standalone.Client()} {
		out := map[string]refusalObservation{}
		for _, s := range steps {
			gateway.saleResponseCode.Store(s.saleResponse)
			customer := openrails.CustomerID(uuid.New())
			session, err := client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
				Customer:       openrails.CheckoutCustomerIdentity{ID: customer, VerifiedEmail: "buyer@example.test", Username: "buyer-" + customer.String()[:8]},
				PriceID:        openrails.PriceID(priceID),
				IdempotencyKey: uuid.NewString(),
				Payment:        s.payment(customer),
			})
			out[s.name] = observeRefusal(t, name+" "+s.name, session, err)
		}
		observed[name] = out
	}

	want := observed["standalone"]
	require.Equal(t, refusalObservation{OK: true, Code: "succeeded"}, want["approved"])
	require.Equal(t, refusalObservation{
		Status: 402, Type: "card_error", Code: "card_declined", DeclineReason: "insufficient_funds", FailureCode: "insufficient_funds",
		Refused: true, CardDeclined: true,
	}, want["insufficient_funds"])
	require.Equal(t, refusalObservation{
		Status: 502, Type: "api_error", Code: "payment_provider_rejected", DeclineReason: "processor_error", FailureCode: "transaction_was_rejected_by_gateway",
		ProviderRej: true, Internal: true,
	}, want["gateway_rejected"])
	require.Equal(t, refusalObservation{
		Status: 402, Type: "card_error", Code: "payment_method_stale", Refused: true, Stale: true,
	}, want["stale_saved_card"])
	require.Equal(t, want, observed["embedded"], "embedded checkout refusals diverged from standalone")
	require.EqualValues(t, 6, gateway.vaults.Load(), "every token checkout vaulted the card at the loopback gateway")
}
