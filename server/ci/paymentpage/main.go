//go:build e2e && integration

// Command paymentpage runs the standalone server with its payment page for
// web/pay's Playwright suite: one merchant on the loopback NMI gateway
// and the fake Stripe, a Turnstile captcha account, the test's clock, the API
// on one origin and the payment page on another. Test-only: /__test/* on the API
// origin makes merchant orders with a pay URL (as a backend does through
// POST /v1/app/orders), stands in for Stripe.js's calls to Stripe, runs the
// payment origin's payment-method domain registration, moves the clock and
// reads host events.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/openrailstest/stripemock"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/operator"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4791", "listen address")
	assets := flag.String("assets", "", "web/pay's build")
	dsn := flag.String("dsn", "", "Postgres DSN")
	lifetime := flag.Duration("lifetime", 30*time.Minute, "exit after this long")
	flag.Parse()
	if err := run(*addr, *assets, *dsn, *lifetime); err != nil {
		log.Fatal(err)
	}
}

type harness struct {
	srv    *server.Server
	rt     *app.Runtime
	clock  *clockwork.FakeClock
	nmi    *nmimock.Mock
	stripe *stripemock.Mock
	mid    billing.MerchantID
	// shop is the merchant's handle on the server's engine.
	shop *openrails.Client
	api  string
	// domain is the payment origin's name.
	domain string
	// payment is the payment origin; other is a site the merchant did not
	// register for its embedded pay URLs.
	payment, other string
}

func run(addr, assets, dsn string, lifetime time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	// Two origins on one listener: the API at localhost, the payment page at
	// 127.0.0.1.
	api, payment := "http://localhost:"+port, "http://127.0.0.1:"+port
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	keys, err := os.MkdirTemp("", "paymentpage-keys-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(keys)

	h := &harness{clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second)), api: api, payment: payment, domain: "127.0.0.1"}
	h.nmi = nmimock.New(nmimock.Options{Clock: h.clock.Now})
	defer h.nmi.Close()
	h.stripe = stripemock.New(stripemock.Options{Clock: h.clock.Now})
	defer h.stripe.Close()
	srv, err := server.New(ctx, server.Config{
		Engine: openrails.Config{
			PublicBillingBaseURL: api, RateLimitsDisabled: true, ProviderWriteMode: openrails.ProviderWritesFull,
			ProviderSandbox: &openrails.ProviderSandboxConfig{NMIGatewayURL: h.nmi.URL(), StripeAPIURL: h.stripe.URL()},
			// Cloudflare's always-passes test keys: the page loads the widget,
			// which the browser suite stands in for.
			Captcha: &openrails.CaptchaConfig{Provider: "turnstile", SiteKey: "1x00000000000000000000AA", SecretKey: "1x0000000000000000000000000000000AA"},
			Merchants: map[string]openrails.MerchantDeclaration{"shop": {DisplayName: "Purchase Shop", // The shop's own site, the API origin, frames its embedded payment pages.
				MerchantSettings: billing.MerchantSettings{PaymentPage: &billing.MerchantPaymentPageSettings{EmbedOrigins: []string{api}}}, PSPs: map[string]openrails.PSPConfig{
					"nmi":    openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "e2e", SecurityKey: "e2e", WebhookSigningSecret: "e2e", TokenizationKey: "e2e-tokenization"}.PSPConfig(),
					"stripe": openrails.StripePSP{Environment: openrails.EnvironmentTest, AccountID: "acct_e2e", SecretKey: "sk_test_acct_e2e", WebhookSigningSecret: "whsec_e2e", PublishableKey: "pk_test_e2e"}.PSPConfig(),
				}}},
		},
		Auth: authkit.Config{
			Token:        authkit.TokenConfig{Issuer: api},
			Keys:         authkit.KeysConfig{Path: keys, AllowEphemeralDevKeys: true},
			Registration: authkit.RegistrationConfig{AllowMissingSenders: true},
			HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
		},
		RouteGroups:   openrails.RouteGroups{Admin: true, Programmatic: true},
		PaymentOrigin: payment,
		Addr:          addr,
	}, server.Deps{Engine: openrails.Deps{Postgres: pool, Clock: h.clock}, PaymentPageAssets: os.DirFS(assets)})
	if err != nil {
		return err
	}
	defer srv.Close(context.WithoutCancel(ctx))
	graph, _ := operator.Of(srv)
	h.srv, h.rt = srv, graph.Runtime
	if err := h.rt.InitRiver(ctx); err != nil {
		return err
	}
	declared := h.rt.DeclaredMerchants()
	if len(declared) != 1 {
		return errors.New("paymentpage: the server declares no merchant")
	}
	h.mid = declared[0]
	if h.shop, err = srv.Engine().Merchant(h.mid); err != nil {
		return err
	}
	h.stripe.SendWebhooksTo(api+"/v1/webhooks/stripe/acct_e2e", "whsec_e2e")

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	// Another site, which the shop did not register.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	h.other = "http://localhost:" + strconv.Itoa(other.Addr().(*net.TCPAddr).Port)
	otherMux := http.NewServeMux()
	otherMux.HandleFunc("GET /__test/embed", h.embed)
	otherServer := &http.Server{Handler: otherMux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = otherServer.Serve(other) }()
	defer otherServer.Close()
	mux.HandleFunc("GET /__test/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{"api": api, "payment": payment, "other": h.other})
	})
	mux.HandleFunc("GET /__test/embed", h.embed)
	mux.HandleFunc("POST /__test/orders", h.newOrder)
	mux.HandleFunc("POST /__test/clock", h.advance)
	mux.HandleFunc("GET /__test/host-events", h.hostEvents)
	mux.HandleFunc("POST /__test/stripe/payment-methods", h.stripeMethod)
	mux.HandleFunc("POST /__test/stripe/authenticate", h.stripeAuthenticate)
	mux.HandleFunc("GET /__test/stripe/domains", h.stripeDomains)
	mux.HandleFunc("GET /__test/thanks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Thanks</title><h1>Thanks for order %s</h1>", r.URL.Query().Get("order"))
	})
	mux.HandleFunc("GET /__test/frame", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Framer</title><iframe src=%q></iframe>", r.URL.Query().Get("src"))
	})
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	log.Printf("paymentpage: api %s, payment %s", api, payment)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newOrder is a merchant order with a pay URL, as the merchant's backend
// creates one: for the customer named, else a new one.
func (h *harness) newOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID          string `json:"customer_id"`
		SavedPaymentMethods bool   `json:"saved_payment_methods"`
		CancelURL           string `json:"cancel_url"`
		Embedded            bool   `json:"embedded"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := r.Context()
	key := "item-" + uuid.NewString()[:8]
	product, err := h.shop.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Course " + key[5:]})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	price, err := h.shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 12_500_000})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	customer := billing.CustomerID(uuid.New())
	if in.CustomerID != "" {
		if customer, err = billing.ParseCustomerID(in.CustomerID); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	payURL := &billing.OrderPayURLParams{SuccessURL: h.api + "/__test/thanks?order={ORDER_ID}", SavedPaymentMethods: in.SavedPaymentMethods,
		Embedded: in.Embedded}
	if in.CancelURL != "" {
		payURL.CancelURL = &in.CancelURL
	}
	order, err := h.shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, IdempotencyKey: uuid.NewString(),
		Lines: []billing.OrderLineParams{{PriceID: price.ID}}, PayURL: payURL})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reply(w, http.StatusCreated, map[string]string{"order_id": order.ID.String(), "customer_id": customer.String(), "url": *order.PayURL})
}

// embed is a merchant's page framing a pay URL (src), as billing-ui's
// EmbeddedPayment does; it keeps what the payment origin tells it.
func (h *harness) embed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><title>Shop</title><script>window.__messages = []; addEventListener("message", (e) => { if (e.origin === %q) window.__messages.push(e.data) })</script>`+
		`<iframe src=%q allow="payment *" style="width:100%%;height:900px;border:0"></iframe>`, h.payment, r.URL.Query().Get("src"))
}

// stripeMethod is Stripe making a PaymentMethod of the card the customer
// typed: last four "3155" needs the bank's challenge.
func (h *harness) stripeMethod(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Last4 string `json:"last4"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Last4) != 4 {
		reply(w, http.StatusBadRequest, map[string]string{"error": "last4"})
		return
	}
	card := stripemock.Card{Brand: "visa", Last4: in.Last4}
	if in.Last4 == "3155" {
		card.Decline = "auth"
	}
	reply(w, http.StatusOK, map[string]string{"id": h.stripe.NewPaymentMethod(card)})
}

// stripeAuthenticate is the customer passing the bank's challenge for a
// PaymentIntent's client secret.
func (h *harness) stripeAuthenticate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pi, _, _ := strings.Cut(in.ClientSecret, "_secret_")
	reply(w, http.StatusOK, map[string]any{"authenticated": h.stripe.Authenticate(pi), "payment_intent": pi})
}

// stripeDomains runs the payment origin's queued registrations as the
// workers would, then answers the payment-method domains the fake Stripe
// holds for the merchant's account.
func (h *harness) stripeDomains(w http.ResponseWriter, r *http.Request) {
	ctx := merchant.WithID(r.Context(), h.mid)
	scopes, err := h.rt.Merchants.ActivePSPScopesForRail(ctx, h.mid, string(models.RailStripe))
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, scope := range scopes {
		row, err := intents.NewStore(h.rt.DB).GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(scope.ID, h.domain))
		if db.IsNotFound(err) {
			continue
		}
		if err == nil && row.Status != intents.StatusSucceeded {
			_, err = h.rt.IntentRunner().ExecuteByID(ctx, row.ID)
		}
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, h.stripe.URL()+"/v1/payment_method_domains", nil)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer sk_test_acct_e2e")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer res.Body.Close()
	var page struct {
		Data []struct {
			Domain string `json:"domain_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	domains := []string{}
	for _, d := range page.Data {
		domains = append(domains, d.Domain)
	}
	reply(w, http.StatusOK, map[string]any{"domains": domains})
}

// advance moves the clock, then expires what is due.
func (h *harness) advance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Seconds int64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Seconds <= 0 {
		reply(w, http.StatusBadRequest, map[string]string{"error": "seconds > 0"})
		return
	}
	h.clock.Advance(time.Duration(in.Seconds) * time.Second)
	n, err := h.rt.Orders.ExpireDue(merchant.WithID(r.Context(), h.mid), 200)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reply(w, http.StatusOK, map[string]any{"now": h.clock.Now(), "expired": n})
}

func (h *harness) hostEvents(w http.ResponseWriter, r *http.Request) {
	page, err := h.shop.ListHostEvents(r.Context(), billing.HostEventListParams{Type: billing.HostEventType(strings.TrimSpace(r.URL.Query().Get("type")))})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reply(w, http.StatusOK, page)
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
