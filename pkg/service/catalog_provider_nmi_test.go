package service

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
)

func intPtr(i int) *int { return &i }

// newMobiusAdapterWithServer builds a nmiAdapter whose NMI client points at
// the given test server for both direct-post and query traffic.
func newMobiusAdapterWithServer(t *testing.T, serverURL string) *nmiAdapter {
	t.Helper()
	svc := &Service{rt: &app.Runtime{
		Config: &config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull},
		RailConfigs: railresolve.FixedSet{"mobius": {
			Rail:      models.RailNMI,
			AccountID: "mobius",
			NMI:       &config.NMIRailConfig{SecurityKey: "test-security-key"},
		}},
	}}
	return &nmiAdapter{svc: svc, testEndpointURL: serverURL}
}

func TestMobiusAdapter_DeterministicPlanIDFormat(t *testing.T) {
	got := nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(30))
	want := "premium-usd-23000000-30"
	if got != want {
		t.Fatalf("plan_id format drift: got %q want %q", got, want)
	}
	// No "openrails-"/merchant/application prefix: the content key IS the whole id.
	if strings.HasPrefix(got, "openrails-") {
		t.Fatalf("generated plan_id must not carry an openrails- prefix: %q", got)
	}
	// Content-addressed: no price-UUID input, so it is stable across a fresh DB;
	// unchanged by cosmetic edits; distinct when money terms change.
	if nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(30)) != want {
		t.Error("plan_id must be deterministic for identical content")
	}
	if nmiDeterministicPlanID("premium", "usd", 29_000_000, intPtr(30)) == want {
		t.Error("a different amount must yield a different plan_id")
	}
	if nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(365)) == want {
		t.Error("a different cycle must yield a different plan_id")
	}
	if nmiDeterministicPlanID("basic", "usd", 23_000_000, intPtr(30)) == want {
		t.Error("a different product key must yield a different plan_id")
	}
}

func TestMobiusAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	// No NMI client configured -> fall back to manual link.
	a := &nmiAdapter{svc: &Service{rt: &app.Runtime{}}}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), BillingCycleDays: intPtr(30),
	})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
}

func TestMobiusAdapter_AutoCreateRejectsNilFrequency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), UnitAmount: 9_990_000, BillingCycleDays: nil,
	})
	if err == nil {
		t.Fatal("expected error when recurring day cadence is nil")
	}
}

func TestMobiusAdapter_AutoCreateFreshCreate(t *testing.T) {
	var addCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/plans":
			addCalled = true
			var req struct {
				PlanAmount   float64 `json:"plan_amount"`
				DayFrequency int     `json:"day_frequency"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.DayFrequency != 30 {
				t.Errorf("day_frequency: got %d want 30", req.DayFrequency)
			}
			if req.PlanAmount != 9.99 {
				t.Errorf("plan_amount: got %v want 9.99", req.PlanAmount)
			}
			_, _ = w.Write([]byte(`{"object":"plan","id":"x"}`))
		case r.Method == http.MethodGet:
			// plan lookup -> not found
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	priceID := uuid.New()
	ids, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: priceID, ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !addCalled {
		t.Fatal("expected AddRecurringPlan to be called for a missing plan")
	}
	if ids[models.RailKeyPlanID] != nmiDeterministicPlanID("pro", "usd", 9_990_000, intPtr(30)) {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
	if ids[models.RailKeyProvider] != "mobius" {
		t.Fatalf("provider mismatch: %v", ids)
	}
}

// TestMobiusAdapter_AutoCreateTargetsSecondaryAccount proves #641 catalog
// secondary-sync routing: with a TargetAccountID set, AutoCreate uses THAT
// account's NMI client (not the primary alias), so a catalog push reaches the
// secondary account.
func TestMobiusAdapter_AutoCreateTargetsSecondaryAccount(t *testing.T) {
	// #788: clients arm from the armed rail state per account; the account a
	// call targets is proven by the credential it arrives with.
	var seenKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/plans" {
			seenKeys = append(seenKeys, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"object":"plan","id":"x"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`))
	}))
	t.Cleanup(srv.Close)

	svc := &Service{rt: &app.Runtime{
		Config: &config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull},
		RailConfigs: railresolve.FixedSet{
			"mobius": {Rail: models.RailNMI, AccountID: "100001", NMI: &config.NMIRailConfig{SecurityKey: "primary-key"}},
			"backup": {Rail: models.RailNMI, AccountID: "100002", Archived: true, NMI: &config.NMIRailConfig{SecurityKey: "secondary-key"}},
		},
	}}
	a := &nmiAdapter{svc: svc, testEndpointURL: srv.URL}

	if _, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000,
		BillingCycleDays: intPtr(30), TargetAccountID: "100002",
	}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(seenKeys) == 0 {
		t.Fatal("secondary account's client must receive the plan create")
	}
	for _, k := range seenKeys {
		if k != "Bearer secondary-key" && k != "secondary-key" {
			t.Fatalf("plan create must carry the TARGET account's credential, got %q", k)
		}
	}
}

func TestMobiusAdapter_AutoCreateAttachNoDuplicate(t *testing.T) {
	// A FRESH-DB price gets a new random UUID, but the content-addressed plan_id is
	// unchanged, so find-or-attach reattaches to the existing NMI plan (no create).
	priceID := uuid.New()
	planID := nmiDeterministicPlanID("pro", "usd", 9_990_000, intPtr(30))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("AddRecurringPlan should NOT be called when plan already exists")
		}
		// plan lookup -> found
		_, _ = w.Write([]byte(nmiPlanQueryJSON(planID, "Premium", "9.99", "")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: priceID, ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyPlanID] != planID {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
}

// nmiPlanQueryJSON renders a single v5 plan GET response.
func nmiPlanQueryJSON(planID, planName, planAmount, dayFrequency string) string {
	return `{"object":"plan","id":"` + planID + `","plan_name":"` + planName +
		`","plan_amount":"` + planAmount + `","day_frequency":"` + dayFrequency + `"}`
}

func TestMobiusAdapter_AttachValidatesLinkAndCreatesNothing(t *testing.T) {
	// A supplied link to a plan that EXISTS and MATCHES the price money terms is
	// accepted, and Attach must never create a plan (no add_plan).
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("Attach must not create an NMI plan when a valid link is supplied")
		}
		_, _ = w.Write([]byte(nmiPlanQueryJSON(planID, "Premium", "9.99", "30")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{ProductKey: "premium", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err != nil {
		t.Fatalf("valid link should attach cleanly, got %v", err)
	}
	if ids[models.RailKeyPlanID] != planID || ids[models.RailKeyProvider] != "mobius" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestMobiusAdapter_AttachCreatesMissingPlanAtOperatorID(t *testing.T) {
	// NMI plan_ids are client-creatable, so a link to a not-yet-existing plan_id
	// is a find-or-CREATE at that operator-chosen id (e.g. "premium").
	var addCalled bool
	var addedPlanID, addedFreq, addedAmount string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/plans" {
			addCalled = true
			var req struct {
				PlanID       string          `json:"id"`
				PlanAmount   json.RawMessage `json:"plan_amount"`
				DayFrequency int             `json:"day_frequency"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			addedPlanID = req.PlanID
			addedFreq = strconv.Itoa(req.DayFrequency)
			addedAmount = string(req.PlanAmount)
			_, _ = w.Write([]byte(`{"object":"plan","id":"premium"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`)) // lookup: not found
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: "premium"},
		autoCreateContext{ProductKey: "premium", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err != nil {
		t.Fatalf("missing plan should be created, got %v", err)
	}
	if !addCalled {
		t.Fatal("expected AddRecurringPlan to be called for a missing operator-supplied plan_id")
	}
	if addedPlanID != "premium" || addedFreq != "30" || addedAmount != "9.99" {
		t.Fatalf("created plan with wrong terms: id=%q freq=%q amount=%q", addedPlanID, addedFreq, addedAmount)
	}
	if ids[models.RailKeyPlanID] != "premium" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestMobiusAdapter_AttachMissingPlanRequiresCycleToCreate(t *testing.T) {
	// A missing plan_id with no billing cycle cannot be created (NMI plans need a
	// frequency) -> loud, actionable error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("must not create an NMI plan without a billing cycle")
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`)) // lookup: not found
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: "premium"},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: nil})
	if err == nil || !strings.Contains(err.Error(), "recurring day cadence") {
		t.Fatalf("expected a loud recurring day cadence error, got %v", err)
	}
}

func TestMobiusAdapter_AttachRejectsAmountMismatch(t *testing.T) {
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Remote plan bills 5.00 (500 cents) but the catalog price is 9_990_000 micros.
		_, _ = w.Write([]byte(nmiPlanQueryJSON(planID, "Premium", "5.00", "30")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("expected an amount-mismatch error, got %v", err)
	}
}

func TestMobiusAdapter_AttachRejectsCycleMismatch(t *testing.T) {
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Remote plan bills every 365 days but the catalog price is 30-day.
		_, _ = w.Write([]byte(nmiPlanQueryJSON(planID, "Premium", "9.99", "365")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err == nil || !strings.Contains(err.Error(), "billing cycle") {
		t.Fatalf("expected a billing-cycle-mismatch error, got %v", err)
	}
}

func TestMobiusAdapter_UpdateIsNoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no NMI request expected: NMI plan Update is a no-op")
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	if err := a.Update(context.Background(), ids, mutableUpdate{}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestMobiusAdapter_VerifyDetectsDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(nmiPlanQueryJSON("p", "Remote Name", "5.00", "")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	drift, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{
		UnitAmount: 9_990_000,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if missing {
		t.Fatal("plan should not be reported missing")
	}
	fields := map[string]string{}
	for _, d := range drift {
		fields[d.Field] = d.RemoteValue
	}
	if fields["unit_amount"] != "5000000" {
		t.Fatalf("expected unit_amount drift (micros), got %v", drift)
	}
}

func TestMobiusAdapter_VerifyMissingPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	_, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !missing {
		t.Fatal("expected missing=true for absent plan")
	}
}

func TestMobiusAdapter_VerifyUnconfiguredIsSyncDisabled(t *testing.T) {
	a := &nmiAdapter{svc: &Service{rt: &app.Runtime{}}}
	drift, missing, err := a.Verify(context.Background(), map[string]string{models.RailKeyPlanID: "p"}, &priceVerifyContext{})
	if err != nil || missing || drift != nil {
		t.Fatalf("expected sync_disabled (nil,false,nil), got drift=%v missing=%v err=%v", drift, missing, err)
	}
}
