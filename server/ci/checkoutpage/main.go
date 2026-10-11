//go:build e2e && integration

// Command checkoutpage runs the standalone server with its hosted checkout for
// web/checkout's Playwright suite: one merchant on the loopback NMI gateway
// and the fake Stripe, a Turnstile captcha account, the test's clock, the API
// on one origin and the checkout on another. Test-only: /__test/* on the API
// origin makes merchant orders with a checkout (as a backend does through
// POST /v1/app/orders), stands in for Stripe.js's calls to Stripe, moves the
// clock and reads host events.
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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/openrailstest/stripemock"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/operator"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4791", "listen address")
	assets := flag.String("assets", "", "web/checkout's build")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres DSN")
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
	api    string
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
	// Two origins on one listener: the API at localhost, the checkout at
	// 127.0.0.1.
	api, checkout := "http://localhost:"+port, "http://127.0.0.1:"+port
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	keys, err := os.MkdirTemp("", "checkoutpage-keys-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(keys)

	h := &harness{clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second)), api: api}
	h.nmi = nmimock.New(nmimock.Options{Clock: h.clock.Now})
	defer h.nmi.Close()
	h.stripe = stripemock.New(stripemock.Options{Clock: h.clock.Now})
	defer h.stripe.Close()
	srv, err := server.New(ctx, server.Config{
		Engine: openrails.Config{
			TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
			PublicBillingBaseURL: api, RateLimitsDisabled: true,
			ProviderSandbox: &openrails.ProviderSandboxConfig{NMIGatewayURL: h.nmi.URL(), StripeAPIURL: h.stripe.URL()},
			// Cloudflare's always-passes test keys: the page loads the widget,
			// which the browser suite stands in for.
			Captcha: &openrails.CaptchaConfig{Provider: "turnstile", SiteKey: "1x00000000000000000000AA", SecretKey: "1x0000000000000000000000000000000AA"},
			Merchant: openrails.MerchantDeclaration{Slug: "shop", DisplayName: "Checkout Shop", PSPs: map[string]openrails.PSPConfig{
				"nmi":    openrails.NMIPSP{AccountID: "e2e", SecurityKey: "e2e", WebhookSigningSecret: "e2e", TokenizationKey: "e2e-tokenization"}.PSPConfig(),
				"stripe": openrails.StripePSP{AccountID: "acct_e2e", SecretKey: "sk_test_acct_e2e", WebhookSigningSecret: "whsec_e2e", PublishableKey: "pk_test_e2e"}.PSPConfig(),
			}},
		},
		Auth: authkit.Config{
			Token:        authkit.TokenConfig{Issuer: api},
			Keys:         authkit.KeysConfig{Path: keys, AllowEphemeralDevKeys: true},
			Registration: authkit.RegistrationConfig{AllowMissingSenders: true},
			HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
		},
		RouteGroups:       openrails.RouteGroups{Admin: true, Programmatic: true},
		HostedCheckoutURL: checkout,
		Addr:              addr,
	}, server.Deps{Engine: openrails.Deps{Postgres: pool, Clock: h.clock}, CheckoutAssets: os.DirFS(assets)})
	if err != nil {
		return err
	}
	defer srv.Close(context.WithoutCancel(ctx))
	graph, _ := operator.Of(srv)
	h.srv, h.rt = srv, graph.Runtime
	if err := h.rt.InitRiver(ctx); err != nil {
		return err
	}
	h.mid = h.rt.ConfiguredMerchant()
	if h.mid.IsZero() {
		return errors.New("checkoutpage: no configured merchant")
	}
	h.stripe.SendWebhooksTo(api+"/v1/webhooks/stripe/acct_e2e", "whsec_e2e")

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	mux.HandleFunc("GET /__test/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{"api": api, "checkout": checkout})
	})
	mux.HandleFunc("POST /__test/orders", h.newOrder)
	mux.HandleFunc("POST /__test/clock", h.advance)
	mux.HandleFunc("GET /__test/host-events", h.hostEvents)
	mux.HandleFunc("POST /__test/stripe/payment-methods", h.stripeMethod)
	mux.HandleFunc("POST /__test/stripe/authenticate", h.stripeAuthenticate)
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
	log.Printf("checkoutpage: api %s, checkout %s", api, checkout)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newOrder is a merchant order with a checkout, as the merchant's backend
// creates one: for the customer named, else a new one.
func (h *harness) newOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID          string `json:"customer_id"`
		SavedPaymentMethods bool   `json:"saved_payment_methods"`
		CancelURL           string `json:"cancel_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, at := r.Context(), openrails.ForMerchantID(h.mid)
	key := "item-" + uuid.NewString()[:8]
	product, err := h.srv.Client().CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Course " + key[5:]}, at)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	price, err := h.srv.Client().CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 12_500_000}, at)
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
	checkout := &billing.OrderCheckoutParams{SuccessURL: h.api + "/__test/thanks?order={ORDER_ID}", SavedPaymentMethods: in.SavedPaymentMethods}
	if in.CancelURL != "" {
		checkout.CancelURL = &in.CancelURL
	}
	order, err := h.srv.Client().CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, IdempotencyKey: uuid.NewString(),
		Lines: []billing.OrderLineParams{{PriceID: price.ID}}, Checkout: checkout}, at)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reply(w, http.StatusCreated, map[string]string{"order_id": order.ID.String(), "customer_id": customer.String(), "url": *order.Checkout.URL})
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
	page, err := h.srv.Client().ListHostEvents(r.Context(), billing.HostEventListParams{Type: billing.HostEventType(strings.TrimSpace(r.URL.Query().Get("type")))}, openrails.ForMerchantID(h.mid))
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
