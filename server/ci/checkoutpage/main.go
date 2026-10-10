//go:build e2e && integration

// Command checkoutpage runs the standalone server with its hosted checkout for
// web/checkout's Playwright suite: one merchant on the loopback NMI gateway,
// the test's clock, the API on one origin and the checkout on another.
// Test-only: /__test/* on the API origin makes merchant orders with a
// checkout, moves the clock and reads host events.
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

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/openrailstest/nmimock"
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
	srv     *server.Server
	rt      *app.Runtime
	clock   *clockwork.FakeClock
	nmi     *nmimock.Mock
	mid     billing.MerchantID
	api     string
	created int
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
	srv, err := server.New(ctx, server.Config{
		Engine: openrails.Config{
			TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
			PublicBillingBaseURL: api, RateLimitsDisabled: true,
			ProviderSandbox: &openrails.ProviderSandboxConfig{NMIGatewayURL: h.nmi.URL()},
			Merchant: openrails.MerchantDeclaration{Slug: "shop", DisplayName: "Checkout Shop", PSPs: map[string]openrails.PSPConfig{
				"nmi": openrails.NMIPSP{AccountID: "e2e", SecurityKey: "e2e", WebhookSigningSecret: "e2e", TokenizationKey: "e2e-tokenization"}.PSPConfig(),
			}},
		},
		Auth: server.AuthConfig{Issuer: api, AllowMissingSenders: true, AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: keys},
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

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	mux.HandleFunc("GET /__test/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{"api": api, "checkout": checkout})
	})
	mux.HandleFunc("POST /__test/orders", h.newOrder)
	mux.HandleFunc("POST /__test/clock", h.advance)
	mux.HandleFunc("GET /__test/host-events", h.hostEvents)
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

// newOrder is a merchant order for a new customer, with a checkout.
func (h *harness) newOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SavedPaymentMethods bool   `json:"saved_payment_methods"`
		CancelURL           string `json:"cancel_url"`
		Amount              int64  `json:"amount,string"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.Amount == 0 {
		in.Amount = 12_500_000
	}
	ctx := merchant.WithID(r.Context(), h.mid)
	h.created++
	key := fmt.Sprintf("item-%d-%s", h.created, uuid.NewString()[:8])
	product, err := h.srv.Client().CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Course " + key[:6]})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	price, err := h.srv.Client().CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: in.Amount})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	customer := uuid.New()
	order, err := h.rt.Orders.Create(ctx, orders.CreateInput{CustomerID: customer, Origin: billing.OrderOriginMerchant,
		Lines: []billing.OrderLineParams{{PriceID: price.ID}}, TTL: hostedcheckout.DefaultLifetime})
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	params := billing.OrderCheckoutParams{SuccessURL: h.api + "/__test/thanks?order={ORDER_ID}", SavedPaymentMethods: in.SavedPaymentMethods}
	if in.CancelURL != "" {
		params.CancelURL = &in.CancelURL
	}
	plan, err := hostedcheckout.Plan(params, billing.OrderID(order.ID), h.clock.Now(), &order.ExpiresAt)
	if err == nil {
		var url string
		url, err = hostedcheckout.Attach(ctx, h.rt.DB.Gen(ctx), h.rt.HostedCheckoutOrigin, h.mid, plan, h.clock.Now())
		if err == nil {
			reply(w, http.StatusCreated, map[string]string{"order_id": billing.OrderID(order.ID).String(), "customer_id": customer.String(), "url": url})
			return
		}
	}
	reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
