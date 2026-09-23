package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
)

// newUnconfiguredService returns a *Service whose runtime has no Stripe config,
// so the stripe adapter's AutoCreate returns errPendingManualLink. Suitable for
// exercising the dispatcher logic without any network / DB access.
func newUnconfiguredService() *Service {
	return &Service{rt: &app.Runtime{}}
}

// -- Per-provider adapter unit tests -----------------------------------------

func TestCCBillAdapter_Attach(t *testing.T) {
	a := &ccbillAdapter{}
	ids, err := a.Attach(context.Background(), map[string]string{
		models.RailKeyCCBillFormName: "premium",
		models.RailKeyCCBillFlexID:   "abc-123",
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyCCBillFormName] != "premium" || ids[models.RailKeyCCBillFlexID] != "abc-123" {
		t.Fatalf("unexpected ids: %v", ids)
	}

	if _, err := a.Attach(context.Background(), map[string]string{"form_name": "premium"}, autoCreateContext{}); err == nil {
		t.Fatal("expected error when flex_id missing")
	}

	// #601: Recurring Billing Option id alone (legacy/archived tier, no FlexForm).
	ids, err = a.Attach(context.Background(), map[string]string{
		models.RailKeyCCBillRecurringBillingOption: "0000000931",
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("RBO-only link must be accepted: %v", err)
	}
	if ids[models.RailKeyCCBillRecurringBillingOption] != "0000000931" {
		t.Fatalf("RBO not preserved: %v", ids)
	}
	if _, ok := ids[models.RailKeyCCBillFlexID]; ok {
		t.Fatalf("RBO-only link must not invent a FlexForm: %v", ids)
	}

	// RBO + FlexForm together (current $23 plan).
	ids, err = a.Attach(context.Background(), map[string]string{
		models.RailKeyCCBillFormName:               "basic-monthly",
		models.RailKeyCCBillFlexID:                 "abc-123",
		models.RailKeyCCBillRecurringBillingOption: "0000007498",
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("RBO + FlexForm must be accepted: %v", err)
	}
	if ids[models.RailKeyCCBillRecurringBillingOption] != "0000007498" || ids[models.RailKeyCCBillFlexID] != "abc-123" {
		t.Fatalf("RBO + FlexForm not both preserved: %v", ids)
	}

	// Empty link (no FlexForm, no RBO) is still an error.
	if _, err := a.Attach(context.Background(), map[string]string{}, autoCreateContext{}); err == nil {
		t.Fatal("expected error for an empty ccbill link")
	}
}

// -- Dispatcher (resolveProviders) tests -------------------------------------

func TestResolveProviders_MixedLinkedAndPending(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	priceID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  openrails.ProductID(productID),
		UnitAmount: 9_990_000,
		Currency:   "USD",
		PSPs:       []string{"ccbill", "nmi"},
		PSPLinks: map[string]map[string]string{
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
			// mobius intentionally has no link -> pending
		},
	}
	rails, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusLinked {
		t.Errorf("ccbill: expected linked, got %s", states["ccbill"].Status)
	}
	if states["nmi"].Status != ProviderStatusPendingManualLink {
		t.Errorf("mobius: expected pending_manual_link, got %s", states["nmi"].Status)
	}
	if _, ok := rails["nmi"]; ok {
		t.Error("mobius should not have a rails entry while pending")
	}
	if len(pending) != 1 || pending[0].Provider != "nmi" {
		t.Fatalf("expected one mobius pending action, got %v", pending)
	}
}

func TestResolveProviders_UnknownProviderErrors(t *testing.T) {
	// An unknown provider name (e.g. an account name like "mobius" instead of
	// its rail) must fail loudly — silent dropping loses provider links.
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  openrails.ProductID(productID),
		UnitAmount: 9_990_000,
		Currency:   "USD",
		PSPs:       []string{"paypal"}, // not in dispatch table
	}
	_, _, _, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err == nil || !strings.Contains(err.Error(), `unknown provider "paypal"`) {
		t.Fatalf("expected unknown-provider error, got %v", err)
	}
}

// -- Mode-gated catalog writes (#346) ----------------------------------------

// TestResolveProviders_RemoteWritesDisabledDefersAutoCreate verifies that in
// limited/readonly mode the dispatcher never calls AutoCreate: every provider
// slot defers to pending_manual_link with the mode message, and the price
// still applies locally.
func TestResolveProviders_RemoteWritesDisabledDefersAutoCreate(t *testing.T) {
	svc := &Service{rt: &app.Runtime{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeLimited}}}
	priceID := uuid.New()
	rails, states, pending, err := svc.resolveProviders(context.Background(), &models.Product{Key: "premium"}, CreatePriceRequest{
		PSPs:       []string{"stripe", "nmi"},
		UnitAmount: 23_000_000,
		Currency:   "USD",
	}, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(rails) != 0 {
		t.Fatalf("no provider objects may be linked in limited mode, got %v", rails)
	}
	for _, name := range []string{"stripe", "nmi"} {
		st, ok := states[name]
		if !ok || st.Status != ProviderStatusPendingManualLink {
			t.Fatalf("%s: expected pending_manual_link, got %+v", name, st)
		}
		if st.Message != remoteWritesDisabledMessage {
			t.Fatalf("%s: expected mode message, got %q", name, st.Message)
		}
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending actions, got %d", len(pending))
	}
}

// TestMobiusAdapter_AttachMissingPlanDeferredWhenWritesDisabled verifies the
// Attach find-or-create half: an explicit link whose NMI plan does NOT exist
// must defer (errRemoteWritesDisabled), never create, when writes are blocked.
func TestMobiusAdapter_AttachMissingPlanDeferredWhenWritesDisabled(t *testing.T) {
	var sawWrite bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// v5 plan lookup: report the plan as missing.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`))
			return
		}
		sawWrite = true // any non-GET request would be a remote write
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	a := newMobiusAdapterWithServer(t, server.URL)
	cycle := 30
	_, err := a.Attach(context.Background(), map[string]string{models.RailKeyPlanID: "premium-usd-23000000-30"}, autoCreateContext{
		ProductKey: "premium", UnitAmount: 23_000_000, Currency: "USD", BillingCycleDays: &cycle,
		RemoteWritesDisabled: true,
	})
	if !errors.Is(err, errRemoteWritesDisabled) {
		t.Fatalf("expected errRemoteWritesDisabled, got %v", err)
	}
	if sawWrite {
		t.Fatal("adapter performed a direct-post write while writes were disabled")
	}
}

// or#896: a `trial:` first phase declared against a rail with no first-phase
// concept is REFUSED at push. It used to be accepted and dropped — the
// subscriber was charged the full amount immediately on enrolment.
func TestResolveProviders_TrialRefusedOnRailsWithoutFirstPhase(t *testing.T) {
	trialAmount := int64(0)
	trialHours := 7 * 24
	newReq := func(psp string) CreatePriceRequest {
		hours := 30 * 24
		return CreatePriceRequest{
			ProductID:           openrails.ProductID(uuid.New()),
			UnitAmount:          23_000_000,
			Currency:            "usd",
			AccessDurationHours: &hours,
			AutoRenew:           true,
			TrialUnitAmount:     &trialAmount,
			TrialDurationHours:  &trialHours,
			PSPs:                []string{psp},
		}
	}

	for _, psp := range []string{"nmi", "solana"} {
		s := newUnconfiguredService()
		req := newReq(psp)
		_, _, _, err := s.resolveProviders(context.Background(), &models.Product{ID: req.ProductID.UUID(), Key: "premium"}, req, uuid.New())
		if err == nil {
			t.Fatalf("%s: a trial on this rail must be refused, not silently dropped", psp)
		}
		for _, want := range []string{"trial", psp, "silently dropped"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", psp, err, want)
			}
		}
	}

	// The rails that can execute a first phase keep working.
	for _, psp := range []string{"stripe", "ccbill"} {
		s := newUnconfiguredService()
		req := newReq(psp)
		if _, _, _, err := s.resolveProviders(context.Background(), &models.Product{ID: req.ProductID.UUID(), Key: "premium"}, req, uuid.New()); err != nil {
			t.Errorf("%s: trials are supported here, got %v", psp, err)
		}
	}

	// A trial-free price on NMI/Solana is untouched.
	for _, psp := range []string{"nmi", "solana"} {
		s := newUnconfiguredService()
		req := newReq(psp)
		req.TrialUnitAmount, req.TrialDurationHours = nil, nil
		if _, _, _, err := s.resolveProviders(context.Background(), &models.Product{ID: req.ProductID.UUID(), Key: "premium"}, req, uuid.New()); err != nil {
			t.Errorf("%s: a price with no trial must still resolve, got %v", psp, err)
		}
	}
}

func TestEngineCatalogDoesNotCreateProviderMirrors(t *testing.T) {
	s := &Service{rt: &app.Runtime{Config: &config.Config{NewSubscriptionCollectionPolicy: "engine"}}}
	product := &models.Product{ID: uuid.New(), Key: "engine-local"}
	hours := 720
	req := CreatePriceRequest{ProductID: openrails.ProductID(product.ID), UnitAmount: 9990000, Currency: "USD", AccessDurationHours: &hours, AutoRenew: true, PSPs: []string{"stripe", "nmi"}}
	links, states, pending, err := s.resolveProviders(context.Background(), product, req, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 || len(states) != 0 || len(pending) != 0 {
		t.Fatalf("engine-only terms unexpectedly requested provider catalog work: %v %v %v", links, states, pending)
	}
	req.AutoRenew = false
	req.AccessDurationHours = nil
	req.PSPs = []string{"stripe"}
	links, states, pending, err = s.resolveProviders(context.Background(), product, req, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 || len(states) != 0 || len(pending) != 0 {
		t.Fatal("one-time engine price requested a Stripe catalog mirror")
	}
	req.PSPs = []string{"stripe"}
	req.PSPLinks = map[string]map[string]string{"stripe": {models.RailKeyStripePriceID: "price_legacy"}}
	links, _, _, err = s.resolveProviders(context.Background(), product, req, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if links["stripe"][models.RailKeyStripePriceID] != "price_legacy" {
		t.Fatal("explicit legacy reference was lost")
	}
}
