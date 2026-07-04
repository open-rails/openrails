package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
)

// #671 TEST WALL — Stripe catalog push wire pinning: a known micros amount on
// the OpenRails price must become the EXACT unit_amount (cents) in the outbound
// POST /v1/prices form body. All traffic flows through the stripeapi choke
// point to a captured httptest server.

func newStripeAdapterWithServer(serverURL string) *stripeAdapter {
	svc := &Service{rt: &app.Runtime{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull},
		Rails: config.RailMerchantAccountSet{
			"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_wirepin"}},
		},
	}}
	return &stripeAdapter{svc: svc, testBaseURL: serverURL}
}

// stripeCatalogFake answers the AutoCreate call sequence (product search,
// product create, price create) and records the POST /v1/prices form.
type stripeCatalogFake struct {
	t          *testing.T
	priceForm  map[string][]string
	priceCalls int
}

func (f *stripeCatalogFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/products/search":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/products":
			_, _ = w.Write([]byte(`{"id":"prod_wirepin"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/prices":
			f.priceCalls++
			if err := r.ParseForm(); err != nil {
				f.t.Errorf("parse /v1/prices form: %v", err)
			}
			f.priceForm = r.PostForm
			_, _ = w.Write([]byte(`{"id":"price_wirepin"}`))
		default:
			f.t.Errorf("unexpected stripe request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestStripeAdapter_AutoCreate_WirePinsUnitAmountCents(t *testing.T) {
	fake := &stripeCatalogFake{t: t}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	a := newStripeAdapterWithServer(server.URL)

	ids, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID:          uuid.New(),
		ProductID:        uuid.New(),
		Product:          &models.Product{Key: "pro", DisplayName: "Pro"},
		ProductKey:       "pro",
		Currency:         "USD",
		UnitAmount:       19_990_000, // micros
		BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fake.priceCalls != 1 {
		t.Fatalf("expected exactly one POST /v1/prices, got %d", fake.priceCalls)
	}
	// THE pin: 19_990_000 micros ⇒ literal "1999" cents on the wire — never
	// "19990000" (raw micros) and never "19" (major units).
	if got := fake.priceForm["unit_amount"]; len(got) != 1 || got[0] != "1999" {
		t.Fatalf("unit_amount on the wire: got %v want [1999]", got)
	}
	if got := fake.priceForm["currency"]; len(got) != 1 || got[0] != "usd" {
		t.Fatalf("currency on the wire: got %v want [usd] (lowercased)", got)
	}
	if got := fake.priceForm["recurring[interval]"]; len(got) != 1 || got[0] != "month" {
		t.Fatalf("recurring[interval] on the wire: got %v want [month]", got)
	}
	if ids[models.RailKeyStripePriceID] != "price_wirepin" || ids[models.RailKeyStripeProductID] != "prod_wirepin" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestStripeAdapter_AutoCreate_SubCentMicrosErrorNeverRounds(t *testing.T) {
	fake := &stripeCatalogFake{t: t}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	a := newStripeAdapterWithServer(server.URL)

	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID:          uuid.New(),
		ProductID:        uuid.New(),
		Product:          &models.Product{Key: "pro", DisplayName: "Pro"},
		ProductKey:       "pro",
		Currency:         "usd",
		UnitAmount:       19_995_000, // 1999.5 cents: not representable
		BillingCycleDays: intPtr(30),
	})
	if err == nil || !strings.Contains(err.Error(), "whole cents") {
		t.Fatalf("sub-cent micros must error (never round), got %v", err)
	}
	if fake.priceCalls != 0 {
		t.Fatalf("no price create may reach the wire on sub-cent input, got %d calls", fake.priceCalls)
	}
}
