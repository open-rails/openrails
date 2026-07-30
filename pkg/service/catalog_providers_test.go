package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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

func TestCCBillAdapter_AutoCreatePending(t *testing.T) {
	a := &ccbillAdapter{}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
	tmpl := a.PendingActionTemplate(uuid.New())
	if tmpl.Provider != "ccbill" || tmpl.Action != "create_flexform" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
}

func TestMobiusAdapter_Attach(t *testing.T) {
	a := &nmiAdapter{}
	// Unarmed + no override: the link stores plan_id only — no fabricated
	// provider key (#845).
	ids, err := a.Attach(context.Background(), map[string]string{models.RailKeyPlanID: "premium_monthly"}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyPlanID] != "premium_monthly" {
		t.Fatalf("unexpected ids: %v", ids)
	}
	if _, ok := ids[models.RailKeyProvider]; ok {
		t.Fatalf("unarmed attach must not fabricate a provider key: %v", ids)
	}
	// An explicit link override is preserved.
	ids, err = a.Attach(context.Background(), map[string]string{
		models.RailKeyPlanID: "premium_monthly", models.RailKeyProvider: "Mobius",
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyProvider] != "mobius" {
		t.Fatalf("override provider must be preserved (lowercased): %v", ids)
	}
	if _, err := a.Attach(context.Background(), map[string]string{}, autoCreateContext{}); err == nil {
		t.Fatal("expected error when plan_id missing")
	}
}

func TestMobiusAdapter_AutoCreatePending(t *testing.T) {
	a := &nmiAdapter{}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
	tmpl := a.PendingActionTemplate(uuid.New())
	if tmpl.Provider != "nmi" || tmpl.Action != "create_recurring_plan" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
	if tmpl.PatchRequired["provider_links"]["nmi"]["plan_id"] == "" {
		t.Fatalf("expected patch_required to mention plan_id, got %+v", tmpl.PatchRequired)
	}
}

func TestStripeAdapter_AttachRequiresPriceID(t *testing.T) {
	a := &stripeAdapter{svc: newUnconfiguredService()}
	if _, err := a.Attach(context.Background(), map[string]string{}, autoCreateContext{}); err == nil {
		t.Fatal("expected error when price_id missing")
	}
	ids, err := a.Attach(context.Background(), map[string]string{
		models.RailKeyStripePriceID:   "price_xxx",
		models.RailKeyStripeProductID: "prod_yyy",
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyStripePriceID] != "price_xxx" || ids[models.RailKeyStripeProductID] != "prod_yyy" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestStripeAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	a := &stripeAdapter{svc: newUnconfiguredService()}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{PriceID: uuid.New(), ProductID: uuid.New()})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink when stripe unconfigured, got %v", err)
	}
}

// -- Dispatcher (resolveProviders) tests -------------------------------------

func TestResolveProviders_AllLinked(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	priceID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 9_990_000,
		Currency:   "usd",
		PSPs:       []string{"stripe", "ccbill", "nmi"},
		PSPLinks: map[string]map[string]string{
			"stripe": {models.RailKeyStripePriceID: "price_xxx"},
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
			"nmi":    {"plan_id": "premium_monthly"},
		},
	}
	rails, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending actions, got %v", pending)
	}
	for _, name := range []string{"stripe", "ccbill", "nmi"} {
		if states[name].Status != ProviderStatusLinked {
			t.Errorf("%s: expected linked, got %s", name, states[name].Status)
		}
		if len(rails[name]) == 0 {
			t.Errorf("%s: expected rails entry", name)
		}
	}
}

func TestResolveProviders_MixedLinkedAndPending(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	priceID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 9_990_000,
		Currency:   "usd",
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

func TestResolveProviders_SolanaSettlementTokenPendingWhenUnconfigured(t *testing.T) {
	s := newUnconfiguredService()
	hours := 30 * 24
	req := CreatePriceRequest{
		ProductID:           uuid.New(),
		UnitAmount:          23_000_000,
		Currency:            "usd",
		AccessDurationHours: &hours,
		AutoRenew:           true,
		PSPs:                []string{"solana"},
		PSPLinks: map[string]map[string]string{
			"solana": {solanaKeyMintSymbol: "USDC"},
		},
	}

	rails, states, pending, err := s.resolveProviders(
		context.Background(),
		&models.Product{ID: req.ProductID, Key: "premium"},
		req,
		uuid.New(),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(rails) != 0 {
		t.Fatalf("unconfigured Solana should not have a rails entry, got %v", rails)
	}
	if states["solana"].Status != ProviderStatusPendingManualLink {
		t.Fatalf("expected pending_manual_link, got %+v", states["solana"])
	}
	if len(pending) != 1 || pending[0].Provider != "solana" {
		t.Fatalf("expected one Solana pending action, got %v", pending)
	}
}

func TestResolveProviders_AllPending(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 9_990_000,
		Currency:   "usd",
		PSPs:       []string{"ccbill", "nmi"},
	}
	_, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusPendingManualLink || states["nmi"].Status != ProviderStatusPendingManualLink {
		t.Fatalf("expected both pending, got %+v", states)
	}
	if len(pending) != 2 {
		t.Fatalf("expected two pending actions, got %d: %v", len(pending), pending)
	}
}

func TestResolveProviders_UnknownProviderErrors(t *testing.T) {
	// An unknown provider name (e.g. an account name like "mobius" instead of
	// its rail) must fail loudly — silent dropping loses provider links.
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 9_990_000,
		Currency:   "usd",
		PSPs:       []string{"paypal"}, // not in dispatch table
	}
	_, _, _, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err == nil || !strings.Contains(err.Error(), `unknown provider "paypal"`) {
		t.Fatalf("expected unknown-provider error, got %v", err)
	}
}

func TestResolveProviders_LinkOnlyInProviderLinks(t *testing.T) {
	// A provider supplied only via provider_links (absent from Providers) is
	// still attached.
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 9_990_000,
		Currency:   "usd",
		PSPLinks: map[string]map[string]string{
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
		},
	}
	_, states, _, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusLinked {
		t.Fatalf("expected ccbill linked, got %s", states["ccbill"].Status)
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
		Currency:   "usd",
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
		ProductKey: "premium", UnitAmount: 23_000_000, Currency: "usd", BillingCycleDays: &cycle,
		RemoteWritesDisabled: true,
	})
	if !errors.Is(err, errRemoteWritesDisabled) {
		t.Fatalf("expected errRemoteWritesDisabled, got %v", err)
	}
	if sawWrite {
		t.Fatal("adapter performed a direct-post write while writes were disabled")
	}
}
