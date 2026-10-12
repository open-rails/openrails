//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/server"
)

const (
	checkoutHost = "checkout.e2e.test"
	serverHost   = "api.e2e.test"
)

var checkoutPage = fstest.MapFS{
	"index.html":    {Data: []byte("<!doctype html><title>Checkout</title>")},
	"assets/app.js": {Data: []byte("void 0")},
}

// checkoutShop is a standalone server with a hosted checkout, one merchant
// on the loopback NMI gateway, and its backend's credential.
type checkoutShop struct {
	t       *testing.T
	f       *fixture
	srv     *server.Server
	handler http.Handler
	clock   *clockwork.FakeClock
	gateway *nmimock.Mock
	mid     billing.MerchantID
	psp     billing.PSPID
	backend string
}

func newCheckoutShop(t *testing.T) *checkoutShop {
	f := newFixture(t)
	s := &checkoutShop{t: t, f: f, clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))}
	s.gateway = nmimock.New(nmimock.Options{Clock: s.clock.Now})
	t.Cleanup(s.gateway.Close)
	vault := vaulttest.New(t)
	s.srv = f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = vault.Config()
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: s.gateway.URL()}
		cfg.Engine.PublicBillingBaseURL = "https://" + serverHost
		cfg.Engine.RateLimitsDisabled = true
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
		cfg.HostedCheckoutURL = "https://" + checkoutHost
		deps.Engine.Clock = s.clock
		deps.CheckoutAssets = checkoutPage
	})
	var err error
	s.handler, err = standaloneHandler(s.srv)
	require.NoError(t, err)
	entry, err := s.srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: uniqueName("checkout"), DisplayName: "Checkout Shop"})
	require.NoError(t, err)
	s.mid = entry.MerchantID
	psp, err := handle(s.t, s.srv, s.mid).CreatePSP(t.Context(), billing.CreatePSPParams{Key: "nmi", Rail: billing.RailNMI, AccountID: "e2e-" + uuid.NewString()[:8],
		Settings:    map[string]any{"tokenization_key": "e2e-tokenization"},
		Credentials: map[string]string{"security_key": "e2e-key", "webhook_signing_secret": "e2e-nmi-webhook"}})
	require.NoError(t, err)
	s.psp = psp.ID
	issuer := newIssuerKey(t, "https://backend-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	trust(t, s.srv, s.mid, issuer.app(t, "owner", nil))
	s.backend = issuer.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["client_id"] = "backend", "backend"
		c["permissions"] = []string{string(billing.MerchantOrdersManage), string(billing.MerchantCustomerRead)}
		c["exp"] = time.Now().Add(time.Hour).Unix()
	})
	return s
}

// price is a new product's price; each order buys its own, so none is
// already owned.
func (s *checkoutShop) price() billing.PriceID {
	s.t.Helper()
	key := "item-" + uuid.NewString()[:8]
	shop := handle(s.t, s.srv, s.mid)
	product, err := shop.CreateProduct(s.t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Course " + key})
	require.NoError(s.t, err)
	price, err := shop.CreatePrice(s.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 5_000_000})
	require.NoError(s.t, err)
	return price.ID
}

func (s *checkoutShop) call(host, method, path, bearer, key string, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(s.t, json.NewEncoder(&payload).Encode(body))
	}
	req := httptest.NewRequest(method, "https://"+host+path, &payload)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res := httptest.NewRecorder()
	s.handler.ServeHTTP(res, req)
	return res
}

// order is a merchant order with a checkout, made by the backend:
// POST /v1/app/orders.
func (s *checkoutShop) order(customer billing.CustomerID, checkout map[string]any) (billing.Order, string) {
	s.t.Helper()
	res := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{
		"customer_id": customer, "lines": []map[string]any{{"price_id": s.price()}}, "checkout": checkout,
	})
	require.Equal(s.t, http.StatusCreated, res.Code, res.Body.String())
	var o billing.Order
	require.NoError(s.t, json.Unmarshal(res.Body.Bytes(), &o))
	require.NotNil(s.t, o.Checkout)
	require.NotNil(s.t, o.Checkout.URL)
	page, secret, ok := strings.Cut(*o.Checkout.URL, "#")
	require.True(s.t, ok)
	require.Equal(s.t, "https://"+checkoutHost+"/c/"+o.ID.String(), page)
	return o, secret
}

func (s *checkoutShop) pay(secret string, order billing.Order, payment map[string]any) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.call(checkoutHost, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", secret, uuid.NewString(),
		map[string]any{"payment": payment, "expected_total": "5000000"})
}

func refused(t *testing.T, res *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), `"`+code+`"`)
}

func decode[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out), res.Body.String())
	return out
}

// A merchant's backend creates an order with a checkout and gets its URL
// once; a replay of the request answers the same URL, and a staff read never
// shows it. Without its own expiry the order expires with its checkout.
func TestHostedCheckoutOnMerchantOrders(t *testing.T) {
	s := newCheckoutShop(t)
	customer := billing.CustomerID(uuid.New())
	key := uuid.NewString()
	body := map[string]any{"customer_id": customer, "lines": []map[string]any{{"price_id": s.price()}},
		"checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks?order={ORDER_ID}", "cancel_url": "https://shop.e2e.test/cart"}}
	first := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	order := decode[billing.Order](t, first)
	require.NotNil(t, order.Checkout.URL)
	require.Equal(t, "https://shop.e2e.test/thanks?order="+order.ID.String(), order.Checkout.SuccessURL)
	require.Equal(t, s.clock.Now().Add(hostedcheckout.DefaultLifetime), order.Checkout.ExpiresAt)
	require.NotNil(t, order.ExpiresAt)
	require.Equal(t, order.Checkout.ExpiresAt, *order.ExpiresAt, "the order expires with its checkout")
	replay := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	again := decode[billing.Order](t, replay)
	require.NotNil(t, again.Checkout, "durable replay retains checkout settings")
	require.NotNil(t, again.Checkout.URL, "a lost creation response must recover the checkout link")
	require.Equal(t, order.ID, again.ID)
	require.Equal(t, *order.Checkout.URL, *again.Checkout.URL)

	staff := s.call(serverHost, http.MethodGet, "/v1/admin/orders/"+order.ID.String(), s.backend, "", nil)
	require.Equal(t, http.StatusOK, staff.Code, staff.Body.String())
	read := decode[billing.Order](t, staff)
	require.NotNil(t, read.Checkout)
	require.Nil(t, read.Checkout.URL, "the URL is shown once")
	require.NotContains(t, staff.Body.String(), "cks_")

	// An order of its own outlives the checkout.
	longer := s.clock.Now().Add(48 * time.Hour)
	res := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{"customer_id": customer,
		"lines": []map[string]any{{"price_id": s.price()}}, "expires_at": longer, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks"}})
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	kept := decode[billing.Order](t, res)
	require.NotNil(t, kept.ExpiresAt)
	require.Equal(t, longer, *kept.ExpiresAt)
	require.Equal(t, s.clock.Now().Add(hostedcheckout.DefaultLifetime), kept.Checkout.ExpiresAt)

	for name, checkout := range map[string]map[string]any{
		"checkout.success_url": {"success_url": "ftp://shop.e2e.test/thanks"},
		"checkout.cancel_url":  {"success_url": "https://shop.e2e.test/thanks", "cancel_url": "javascript:alert(1)"},
		"checkout.expires_at":  {"success_url": "https://shop.e2e.test/thanks", "expires_at": s.clock.Now().Add(10 * time.Minute)},
	} {
		res := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(),
			map[string]any{"customer_id": customer, "lines": []map[string]any{{"price_id": s.price()}}, "checkout": checkout})
		refused(t, res, http.StatusBadRequest, billing.CodeInvalidParam)
		require.Contains(t, res.Body.String(), `"param":"`+name+`"`)
	}

	// A host serving no checkout refuses one.
	_, embedded := newFixture(t).runtime(t, uniqueName("embedded"))
	_, err := embedded.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, IdempotencyKey: uuid.NewString(),
		Lines: []billing.OrderLineParams{{PriceID: billing.PriceID("test:" + uuid.NewString() + ":v1")}}, Checkout: &billing.OrderCheckoutParams{SuccessURL: "https://shop.e2e.test/thanks"}})
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "hosted_checkout_unavailable", refusal.Code)
}

// Creation retries recover only a live capability, while the order itself is
// read afresh. Time-relative creation rules never reject an unchanged retry.
func TestHostedCheckoutReplayUsesCurrentState(t *testing.T) {
	s := newCheckoutShop(t)
	customer := billing.CustomerID(uuid.New())
	key := uuid.NewString()
	expiry := s.clock.Now().Add(hostedcheckout.MinLifetime)
	body := map[string]any{"customer_id": customer, "lines": []map[string]any{{"price_id": s.price()}}, "expires_at": s.clock.Now().Add(time.Hour),
		"checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks", "expires_at": expiry}}
	created := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	original := decode[billing.Order](t, created)
	require.NotNil(t, original.Checkout.URL)
	// The only persisted recovery data is the secret, bounded by this checkout.
	var receipt []byte
	var retainedUntil time.Time
	query := fmt.Sprintf(`SELECT result,expires_at FROM %s.idempotency_keys WHERE merchant_id=$1 AND operation='hosted_checkout_creation' AND idempotency_key=$2`, s.f.schema)
	require.NoError(t, s.f.pool.QueryRow(t.Context(), query, s.mid.UUID(), original.ID.UUID().String()).Scan(&receipt, &retainedUntil))
	var fields map[string]string
	require.NoError(t, json.Unmarshal(receipt, &fields))
	require.Len(t, fields, 1)
	_, secret, _ := strings.Cut(*original.Checkout.URL, "#")
	require.Equal(t, secret, fields["secret"])
	require.True(t, expiry.Equal(retainedUntil))
	replay := func() billing.Order {
		response := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		order := decode[billing.Order](t, response)
		require.Equal(t, original.ID, order.ID)
		return order
	}
	s.clock.Advance(20 * time.Minute)
	live := replay() // less than the minimum lifetime remains; this is still a retry
	require.Equal(t, original.Checkout.URL, live.Checkout.URL)
	require.True(t, expiry.Equal(live.Checkout.ExpiresAt), "a retry never extends the checkout")
	// A valid-looking capability from elsewhere cannot be substituted into this receipt.
	otherSecret, _, err := hostedcheckout.NewSecret()
	require.NoError(t, err)
	update := fmt.Sprintf(`UPDATE %s.idempotency_keys SET result=$3::jsonb WHERE merchant_id=$1 AND operation='hosted_checkout_creation' AND idempotency_key=$2`, s.f.schema)
	wrong, _ := json.Marshal(map[string]string{"secret": otherSecret})
	_, err = s.f.pool.Exec(t.Context(), update, s.mid.UUID(), original.ID.UUID().String(), wrong)
	require.NoError(t, err)
	require.Nil(t, replay().Checkout.URL, "the capability must match the current order's hash")
	_, err = s.f.pool.Exec(t.Context(), update, s.mid.UUID(), original.ID.UUID().String(), receipt)
	require.NoError(t, err)
	require.Equal(t, original.Checkout.URL, replay().Checkout.URL)

	s.clock.Advance(10*time.Minute + time.Second)
	expired := replay()
	require.Equal(t, billing.OrderOpen, expired.Status, "the order outlives its checkout")
	require.Nil(t, expired.Checkout.URL, "no expired URL is replayed")

	for _, closeOrder := range []string{"pay", "cancel"} {
		t.Run(closeOrder, func(t *testing.T) {
			otherKey := uuid.NewString()
			request := map[string]any{"customer_id": billing.CustomerID(uuid.New()), "lines": []map[string]any{{"price_id": s.price()}}, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks"}}
			response := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, otherKey, request)
			require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
			first := decode[billing.Order](t, response)
			require.NotNil(t, first.Checkout.URL)
			want := billing.OrderCanceled
			if closeOrder == "pay" {
				_, secret, _ := strings.Cut(*first.Checkout.URL, "#")
				paid := s.pay(secret, first, map[string]any{"token": s.gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), "psp_id": s.psp})
				require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
				want = billing.OrderComplete
			} else {
				canceled := s.call(serverHost, http.MethodPost, "/v1/app/orders/"+first.ID.String()+"/cancel", s.backend, "", nil)
				require.Equal(t, http.StatusOK, canceled.Code, canceled.Body.String())
			}
			replayed := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, otherKey, request)
			require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
			current := decode[billing.Order](t, replayed)
			require.Equal(t, want, current.Status, "a replay cannot return the original open response")
			require.Nil(t, current.Checkout.URL, "a closed order's URL is not exposed again")
		})
	}
}

// Only the matching live capability is recovered from a pre-upgrade creation
// receipt. The older response body is never replayed.
func TestHostedCheckoutReplaysLegacyCreationReceipt(t *testing.T) {
	s := newCheckoutShop(t)
	customer := billing.CustomerID(uuid.New())
	key := uuid.NewString()
	body := map[string]any{"customer_id": customer, "lines": []map[string]any{{"price_id": s.price()}}, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks"}}
	first := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	order := decode[billing.Order](t, first)
	_, err := s.f.pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.idempotency_keys WHERE merchant_id=$1 AND operation='hosted_checkout_creation' AND idempotency_key=$2`, s.f.schema), s.mid.UUID(), order.ID.UUID().String())
	require.NoError(t, err)
	stored := map[string]any{"id": order.ID, "customer_id": customer, "status": "expired", "total": "1", "lines": []map[string]any{{"price_id": "price_" + uuid.NewString()}}, "checkout": map[string]any{"url": *order.Checkout.URL}}
	save := func() {
		response, err := json.Marshal(stored)
		require.NoError(t, err)
		receipt, err := json.Marshal(map[string]any{"fingerprint": "historical", "status": 201, "content_type": "application/json", "body": string(response)})
		require.NoError(t, err)
		_, err = s.f.pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.idempotency_keys (merchant_id,operation,idempotency_key,status,token,result,lease_expires_at,expires_at)
VALUES ($1,'POST /v1/app/orders',$2,'succeeded',$3,$4,now(),now()+interval '1 day') ON CONFLICT(merchant_id,operation,idempotency_key) DO UPDATE SET result=EXCLUDED.result`, s.f.schema), s.mid.UUID(), key, uuid.New(), receipt)
		require.NoError(t, err)
	}
	replay := func() billing.Order {
		response := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		current := decode[billing.Order](t, response)
		require.Equal(t, order.ID, current.ID)
		require.Equal(t, billing.OrderOpen, current.Status)
		require.Equal(t, order.Total, current.Total, "the legacy body is not a current order response")
		return current
	}
	save()
	require.Equal(t, order.Checkout.URL, replay().Checkout.URL)
	stored["id"] = billing.OrderID(uuid.New())
	save()
	require.Nil(t, replay().Checkout.URL, "the original receipt must name this exact order")
	stored["id"] = order.ID
	stored["customer_id"] = billing.CustomerID(uuid.New())
	save()
	require.Nil(t, replay().Checkout.URL, "the original receipt must name this customer")
	_, err = s.f.pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.idempotency_keys WHERE merchant_id=$1 AND operation='POST /v1/app/orders' AND idempotency_key=$2`, s.f.schema), s.mid.UUID(), key)
	require.NoError(t, err)
	require.Nil(t, replay().Checkout.URL, "a missing receipt cannot be reconstructed or create a second order")
	var count int
	require.NoError(t, s.f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.orders WHERE merchant_id=$1 AND idempotency_key=$2`, s.f.schema), s.mid.UUID(), "app:"+key).Scan(&count))
	require.Equal(t, 1, count)
}

func TestHostedCheckoutReceiptFailureRollsBackOrder(t *testing.T) {
	s := newCheckoutShop(t)
	body := map[string]any{"customer_id": billing.CustomerID(uuid.New()), "lines": []map[string]any{{"price_id": s.price()}}, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks"}}
	key := uuid.NewString()
	// A receipt persistence failure cannot leave an order whose response is lost
	// and whose capability can never be recovered.
	ddl := fmt.Sprintf(`CREATE FUNCTION %[1]s.refuse_checkout_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.operation='hosted_checkout_creation' THEN RAISE EXCEPTION 'receipt unavailable'; END IF; RETURN NEW; END $$;
CREATE TRIGGER refuse_checkout_receipt BEFORE INSERT ON %[1]s.idempotency_keys FOR EACH ROW EXECUTE FUNCTION %[1]s.refuse_checkout_receipt();`, s.f.schema)
	_, err := s.f.pool.Exec(t.Context(), ddl)
	require.NoError(t, err)
	failed := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
	var count int
	require.NoError(t, s.f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.orders WHERE merchant_id=$1 AND idempotency_key=$2`, s.f.schema), s.mid.UUID(), "app:"+key).Scan(&count))
	require.Zero(t, count, "receipt and order share one transaction")
	_, err = s.f.pool.Exec(t.Context(), fmt.Sprintf(`DROP TRIGGER refuse_checkout_receipt ON %s.idempotency_keys`, s.f.schema))
	require.NoError(t, err)
	recovered := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, key, body)
	require.Equal(t, http.StatusCreated, recovered.Code, recovered.Body.String())
	order := decode[billing.Order](t, recovered)
	require.NotNil(t, order.Checkout.URL)
}

// The checkout host serves the page and, under one checkout URL's secret,
// that order's own routes as its customer: never another order, never saved
// cards the merchant did not offer, never on the API host, and not after the
// URL expires.
func TestHostedCheckoutSecretOpensOneOrder(t *testing.T) {
	s := newCheckoutShop(t)
	customer := billing.CustomerID(uuid.New())
	orderA, secretA := s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks"})
	orderB, secretB := s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks", "saved_payment_methods": true})

	page := s.call(checkoutHost, http.MethodGet, "/c/"+orderA.ID.String(), "", "", nil)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	require.Equal(t, "DENY", page.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", page.Header().Get("Referrer-Policy"))
	require.Equal(t, "no-store", page.Header().Get("Cache-Control"))
	require.Equal(t, http.StatusOK, s.call(checkoutHost, http.MethodGet, "/assets/app.js", "", "", nil).Code)
	require.Equal(t, http.StatusNotFound, s.call(checkoutHost, http.MethodGet, "/c/not-an-order", "", "", nil).Code)
	require.Equal(t, http.StatusNotFound, s.call(checkoutHost, http.MethodGet, "/admin/", "", "", nil).Code)

	own := s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.ID.String(), secretA, "", nil)
	require.Equal(t, http.StatusOK, own.Code, own.Body.String())
	require.Equal(t, "no-store", own.Header().Get("Cache-Control"))
	read := decode[billing.Order](t, own)
	require.Equal(t, orderA.ID, read.ID)
	require.Equal(t, customer, read.CustomerID)
	require.NotEmpty(t, read.PaymentOptions)

	config := s.call(checkoutHost, http.MethodGet, "/v1/me/config", secretA, "", nil)
	require.Equal(t, http.StatusOK, config.Code, config.Body.String())
	require.Equal(t, "no-store", config.Header().Get("Cache-Control"))
	doc := decode[billing.CustomerConfig](t, config)
	require.Len(t, doc.Payment.PSPs, 1, "the secret names its merchant")
	require.Equal(t, "Checkout Shop", doc.Merchant.DisplayName)
	require.Equal(t, http.StatusOK, s.call(checkoutHost, http.MethodGet, "/v1/config", secretA, "", nil).Code)

	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderB.ID.String(), secretA, "", nil), http.StatusNotFound, billing.CodeResourceNotFound)
	refused(t, s.call(checkoutHost, http.MethodPost, "/v1/me/orders/"+orderB.ID.String()+"/confirm", secretA, "", nil), http.StatusNotFound, billing.CodeResourceNotFound)
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders", secretA, "", nil), http.StatusNotFound, billing.CodeRouteNotFound)
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/subscriptions", secretA, "", nil), http.StatusNotFound, billing.CodeRouteNotFound)
	refused(t, s.call(checkoutHost, http.MethodPost, "/v1/me/orders/"+orderA.ID.String()+"/cancel", secretA, "", nil), http.StatusNotFound, billing.CodeRouteNotFound)
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/payment-methods", secretA, "", nil), http.StatusNotFound, billing.CodeResourceNotFound)
	require.Equal(t, http.StatusOK, s.call(checkoutHost, http.MethodGet, "/v1/me/payment-methods", secretB, "", nil).Code)

	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.ID.String(), "", "", nil), http.StatusNotFound, "checkout_not_found")
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/config", "", "", nil), http.StatusNotFound, "checkout_not_found")
	wrong, _, err := hostedcheckout.NewSecret()
	require.NoError(t, err)
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.ID.String(), wrong, "", nil), http.StatusNotFound, "checkout_not_found")
	onAPI := s.call(serverHost, http.MethodGet, "/v1/me/orders/"+orderA.ID.String(), secretA, "", nil)
	require.Equal(t, http.StatusUnauthorized, onAPI.Code, onAPI.Body.String())

	// Past its expiry the URL stops working; the order expires with it.
	s.clock.Advance(hostedcheckout.DefaultLifetime + time.Second)
	refused(t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.ID.String(), secretA, "", nil), http.StatusGone, "checkout_expired")
	rt := engine.Graph(s.srv.Engine()).Runtime
	n, err := rt.Orders.ExpireDue(merchant.WithID(t.Context(), s.mid), 10)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// The customer pays at the checkout with a new card, charged and saved in
// one request; the backend learns of it from order.completed. A later
// checkout shows that card only when the merchant offers saved cards, and
// pays with it only then.
func TestHostedCheckoutPays(t *testing.T) {
	s := newCheckoutShop(t)
	customer := billing.CustomerID(uuid.New())
	first, secret := s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks"})
	refused(t, s.pay(secret, first, map[string]any{"payment_method_id": billing.PaymentMethodID(uuid.New())}), http.StatusNotFound, billing.CodeResourceNotFound)
	paid := s.pay(secret, first, map[string]any{"token": s.gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), "psp_id": s.psp})
	require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
	require.Equal(t, billing.OrderComplete, decode[billing.Order](t, paid).Status)
	events, err := handle(s.t, s.srv, s.mid).ListHostEvents(t.Context(), billing.HostEventListParams{Type: billing.HostEventOrderCompleted})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	require.Equal(t, first.ID, events.Items[0].Order.OrderID)

	offered, secretOffered := s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks", "saved_payment_methods": true})
	cards := decode[billing.ListPage[billing.PaymentMethod]](t, s.call(checkoutHost, http.MethodGet, "/v1/me/payment-methods", secretOffered, "", nil))
	require.Len(t, cards.Items, 1)
	card := cards.Items[0]
	require.True(t, card.Reusable)
	require.Equal(t, "4242", *card.Card.Last4)
	require.Empty(t, card.Mandates, "the checkout shows the card, not what it pays")

	notOffered, secretNotOffered := s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks"})
	refused(t, s.pay(secretNotOffered, notOffered, map[string]any{"payment_method_id": card.ID}), http.StatusNotFound, billing.CodeResourceNotFound)
	saved := s.pay(secretOffered, offered, map[string]any{"payment_method_id": card.ID})
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	require.Equal(t, billing.OrderComplete, decode[billing.Order](t, saved).Status)
}

// One checkout takes at most hostedcheckout.MaxDeclines declined cards,
// whoever presents it: then the merchant makes a new order.
func TestHostedCheckoutDeclinesCap(t *testing.T) {
	s := newCheckoutShop(t)
	order, secret := s.order(billing.CustomerID(uuid.New()), map[string]any{"success_url": "https://shop.e2e.test/thanks"})
	for i := range hostedcheckout.MaxDeclines {
		if i == 6 {
			// Past the customer's burst block; the cap counts the whole order.
			s.clock.Advance(16 * time.Minute)
		}
		declined := s.pay(secret, order, map[string]any{"token": s.gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "0002", Decline: nmimock.InsufficientFunds}), "psp_id": s.psp})
		require.Equal(t, http.StatusPaymentRequired, declined.Code, "attempt %d: %s", i+1, declined.Body.String())
	}
	refused(t, s.pay(secret, order, map[string]any{"token": s.gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), "psp_id": s.psp}),
		http.StatusConflict, "checkout_attempts_exhausted")
}

// The checkout page runs the PSPs' scripts, so it never shares an origin
// with the API, AuthKit or the product's pages.
func TestHostedCheckoutNeedsItsOwnOrigin(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		edit func(*server.Config)
	}{
		{"api", func(cfg *server.Config) { cfg.Engine.PublicBillingBaseURL = "https://" + checkoutHost }},
		{"issuer", func(cfg *server.Config) { cfg.Auth.Token.Issuer = "https://" + checkoutHost + "/auth" }},
		{"frontend", func(cfg *server.Config) { cfg.Auth.Frontend.BaseURL = "https://" + checkoutHost }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
				cfg.HostedCheckoutURL = "https://" + checkoutHost
				deps.CheckoutAssets = checkoutPage
				tc.edit(cfg)
			})
			require.ErrorContains(t, err, "own origin")
		})
	}
	_, err := f.buildServer(t, func(cfg *server.Config, _ *server.Deps) { cfg.HostedCheckoutURL = "https://" + checkoutHost + "/pay" })
	require.ErrorContains(t, err, "no path")
}
