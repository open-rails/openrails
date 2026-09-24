package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// nmiCatalogCtx carries the merchant every NMI client is bound to (#1055).
func nmiCatalogCtx() context.Context {
	return merchant.WithID(context.Background(), merchant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111")))
}

func nmiService(psps railresolve.FixedSet) *Service {
	return &Service{rt: &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull}, RailConfigs: psps}}
}

func newMobiusAdapterWithServer(serverURL string) *nmiAdapter {
	svc := nmiService(railresolve.FixedSet{"mobius": {Rail: models.RailNMI, AccountID: "mobius", NMI: &config.NMIRailConfig{SecurityKey: "test-security-key"}}})
	return &nmiAdapter{svc: svc, testEndpointURL: serverURL}
}

type nmiPlanCreate struct {
	ID           string      `json:"id"`
	PlanAmount   json.Number `json:"plan_amount"` // exact wire text, never a float (#818)
	DayFrequency int         `json:"day_frequency"`
	auth         string
}

// fakeNMIPlans answers v5 plan lookups from `plans` (id -> response JSON) and
// records creates.
func fakeNMIPlans(t *testing.T, plans map[string]string) (*httptest.Server, *[]nmiPlanCreate) {
	var creates []nmiPlanCreate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/plans" {
			var c nmiPlanCreate
			require.NoError(t, json.NewDecoder(r.Body).Decode(&c))
			c.auth = r.Header.Get("Authorization")
			creates = append(creates, c)
			fmt.Fprintf(w, `{"object":"plan","id":%q}`, c.ID)
			return
		}
		if r.Method == http.MethodGet {
			for id, body := range plans {
				if r.URL.Path == "/plans/"+url.PathEscape(id) {
					fmt.Fprint(w, body)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"no plan"}`)
			return
		}
		t.Errorf("unexpected NMI request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, &creates
}

func nmiPlanJSON(id, amount, days string) string {
	return fmt.Sprintf(`{"object":"plan","id":%q,"plan_name":"Remote Name","plan_amount":%q,"day_frequency":%q}`, id, amount, days)
}

func TestNMIAdapterAutoCreateIsContentAddressed(t *testing.T) {
	planID := nmiDeterministicPlanID("pro", "usd", 9_990_000, intPtr(30))
	in := autoCreateContext{PriceID: uuid.New(), ProductKey: "pro", Currency: "USD", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)}

	srv, creates := fakeNMIPlans(t, nil)
	ids, err := newMobiusAdapterWithServer(srv.URL).AutoCreate(nmiCatalogCtx(), in)
	require.NoError(t, err)
	require.Equal(t, map[string]string{models.RailKeyPlanID: planID, models.RailKeyProvider: "mobius"}, ids)
	require.Len(t, *creates, 1)
	require.Equal(t, "9.99", (*creates)[0].PlanAmount.String())
	require.Equal(t, 30, (*creates)[0].DayFrequency)

	// A rebuilt DB (fresh price UUID) re-attaches to the same plan, never duplicates.
	srv, creates = fakeNMIPlans(t, map[string]string{planID: nmiPlanJSON(planID, "9.99", "")})
	in.PriceID = uuid.New()
	ids, err = newMobiusAdapterWithServer(srv.URL).AutoCreate(nmiCatalogCtx(), in)
	require.NoError(t, err)
	require.Equal(t, planID, ids[models.RailKeyPlanID])
	require.Empty(t, *creates)

	// Unarmed NMI defers to a manual link rather than failing the price.
	_, err = (&nmiAdapter{svc: &Service{rt: &app.Runtime{}}}).AutoCreate(nmiCatalogCtx(), in)
	require.ErrorIs(t, err, errPendingManualLink)
}

// #641/#845: a secondary sync reaches THAT account with its own credential,
// and link metadata names the merchant's own PSP key.
func TestNMIAdapterTargetsResolvedAccount(t *testing.T) {
	srv, creates := fakeNMIPlans(t, nil)
	a := &nmiAdapter{svc: nmiService(railresolve.FixedSet{
		"mobius": {Rail: models.RailNMI, AccountID: "100001", NMI: &config.NMIRailConfig{SecurityKey: "primary-key"}},
		"backup": {Rail: models.RailNMI, AccountID: "100002", Archived: true, NMI: &config.NMIRailConfig{SecurityKey: "secondary-key"}},
	}), testEndpointURL: srv.URL}
	ids, err := a.AutoCreate(nmiCatalogCtx(), autoCreateContext{PriceID: uuid.New(), ProductKey: "pro", Currency: "USD", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30), TargetAccountID: "100002"})
	require.NoError(t, err)
	require.Equal(t, "backup", ids[models.RailKeyProvider])
	require.Len(t, *creates, 1)
	require.Contains(t, []string{"Bearer secondary-key", "secondary-key"}, (*creates)[0].auth)

	ids, err = a.Attach(nmiCatalogCtx(), map[string]string{models.RailKeyPlanID: "premium"}, autoCreateContext{Currency: "USD", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	require.NoError(t, err)
	require.Equal(t, "mobius", ids[models.RailKeyProvider], "Attach defaults to the active account's key")
}

func TestNMIAdapterAttachVerifiesOrCreatesAtOperatorID(t *testing.T) {
	in := autoCreateContext{ProductKey: "premium", Currency: "USD", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)}
	for _, tc := range []struct {
		name   string
		remote string // "" = plan missing
		mutate func(*autoCreateContext)
		err    string
		create bool
	}{
		{name: "matching plan", remote: nmiPlanJSON("premium", "9.99", "30")},
		{name: "month-based plan reports no cadence", remote: nmiPlanJSON("premium", "9.99", "0")},
		{name: "amount mismatch", remote: nmiPlanJSON("premium", "5.00", "30"), err: "amount"},
		{name: "cycle mismatch", remote: nmiPlanJSON("premium", "9.99", "365"), err: "billing cycle"},
		{name: "missing plan created at operator id", create: true},
		{name: "missing plan needs a cadence", mutate: func(c *autoCreateContext) { c.BillingCycleDays = nil }, err: "recurring day cadence"},
		{name: "sub-cent amount never rounds", mutate: func(c *autoCreateContext) { c.UnitAmount = 9_995_000 }, err: "could not be created"},
		{name: "writes disabled defers", mutate: func(c *autoCreateContext) { c.RemoteWritesDisabled = true }, err: errRemoteWritesDisabled.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := map[string]string{}
			if tc.remote != "" {
				plans["premium"] = tc.remote
			}
			srv, creates := fakeNMIPlans(t, plans)
			ctx := in
			if tc.mutate != nil {
				tc.mutate(&ctx)
			}
			ids, err := newMobiusAdapterWithServer(srv.URL).Attach(nmiCatalogCtx(), map[string]string{models.RailKeyPlanID: "premium"}, ctx)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				require.Empty(t, *creates, "a refused link never writes")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "premium", ids[models.RailKeyPlanID])
			if tc.create {
				require.Len(t, *creates, 1)
				c := (*creates)[0]
				require.Equal(t, []string{"premium", "9.99", "30"}, []string{c.ID, c.PlanAmount.String(), fmt.Sprint(c.DayFrequency)})
			} else {
				require.Empty(t, *creates)
			}
		})
	}
}

func TestNMIAdapterVerify(t *testing.T) {
	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	srv, _ := fakeNMIPlans(t, map[string]string{"p": nmiPlanJSON("p", "5.00", "")})
	drift, missing, err := newMobiusAdapterWithServer(srv.URL).Verify(nmiCatalogCtx(), ids, &priceVerifyContext{UnitAmount: 9_990_000, Currency: "USD"})
	require.NoError(t, err)
	require.False(t, missing)
	remote := map[string]string{}
	for _, d := range drift {
		remote[d.Field] = d.RemoteValue
	}
	require.Equal(t, "5000000", remote["unit_amount"], "remote cents are compared as micros")

	srv, _ = fakeNMIPlans(t, nil)
	_, missing, err = newMobiusAdapterWithServer(srv.URL).Verify(nmiCatalogCtx(), ids, &priceVerifyContext{Currency: "USD"})
	require.NoError(t, err)
	require.True(t, missing)

	// No readable account is never an in-sync verdict.
	drift, missing, err = (&nmiAdapter{svc: &Service{rt: &app.Runtime{}}}).Verify(nmiCatalogCtx(), ids, &priceVerifyContext{Currency: "USD"})
	require.ErrorIs(t, err, errProviderNotArmed)
	require.False(t, missing)
	require.Nil(t, drift)
}

// The runtime NMI factory must honor the configured sandbox gateway; the
// private test seam takes priority over it.
func TestNMICatalogClientUsesConfiguredSandboxEndpoint(t *testing.T) {
	for _, privateOverride := range []bool{false, true} {
		gateway, _ := fakeNMIPlans(t, map[string]string{"known": `{"object":"plan","id":"known","plan_amount":"23.00","day_frequency":"30","plan_payments":"0"}`})
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("private endpoint must take priority") }))
		t.Cleanup(other.Close)
		a := newMobiusAdapterWithServer("")
		a.svc.rt.Config.TestMode = config.CredentialPostureSandbox
		a.svc.rt.Config.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
		if privateOverride {
			a.testEndpointURL = gateway.URL
			a.svc.rt.Config.ProviderSandbox.NMIGatewayURL = other.URL
		}
		client, _, ok := a.nmiClientFor(nmiCatalogCtx(), "mobius")
		require.True(t, ok)
		require.Equal(t, []string{gateway.URL, gateway.URL, gateway.URL}, []string{client.V5BaseURL, client.QueryURL, client.DirectPostURL})
		detail, err := client.GetRecurringPlanDetailByID(nmiCatalogCtx(), "known", "USD")
		require.NoError(t, err)
		require.True(t, detail.Found)
	}
}

func newStripeAdapterWithServer(serverURL string) *stripeAdapter {
	return &stripeAdapter{svc: &Service{rt: &app.Runtime{
		Config:      &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull},
		RailConfigs: railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_wirepin"}}},
	}}, testBaseURL: serverURL}
}

// #671: micros become the EXACT Stripe unit_amount in cents on the wire, and
// an amount that is not whole cents never rounds or reaches the provider.
func TestStripeAdapterAutoCreateWireAmount(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string // "" = refused before the wire
	}{{19_990_000, "1999"}, {19_995_000, ""}} {
		var priceForms []url.Values
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/products/search":
				fmt.Fprint(w, `{"data":[]}`)
			case r.Method == http.MethodPost && r.URL.Path == "/v1/products":
				fmt.Fprint(w, `{"id":"prod_wirepin"}`)
			case r.Method == http.MethodPost && r.URL.Path == "/v1/prices":
				require.NoError(t, r.ParseForm())
				priceForms = append(priceForms, r.PostForm)
				fmt.Fprint(w, `{"id":"price_wirepin"}`)
			default:
				t.Errorf("unexpected stripe request %s %s", r.Method, r.URL.Path)
			}
		}))
		ids, err := newStripeAdapterWithServer(srv.URL).AutoCreate(context.Background(), autoCreateContext{
			PriceID: uuid.New(), ProductID: uuid.New(), Product: &models.Product{Key: "pro", DisplayName: "Pro"},
			ProductKey: "pro", Currency: "USD", UnitAmount: tc.micros, BillingCycleDays: intPtr(30),
		})
		srv.Close()
		if tc.want == "" {
			require.ErrorContains(t, err, "whole cents")
			require.Empty(t, priceForms)
			continue
		}
		require.NoError(t, err)
		require.Len(t, priceForms, 1)
		for field, want := range map[string]string{"unit_amount": tc.want, "currency": "usd", "recurring[interval]": "month"} {
			require.Equal(t, []string{want}, priceForms[0][field], field)
		}
		require.Equal(t, "price_wirepin", ids[models.RailKeyStripePriceID])
		require.Equal(t, "prod_wirepin", ids[models.RailKeyStripeProductID])
	}
}

func TestCCBillAdapterAttach(t *testing.T) {
	a := &ccbillAdapter{}
	for _, link := range []map[string]string{
		{models.RailKeyCCBillFormName: "premium", models.RailKeyCCBillFlexID: "abc-123"},
		{models.RailKeyCCBillRecurringBillingOption: "0000000931"}, // #601 legacy RBO-only
		{models.RailKeyCCBillFormName: "basic-monthly", models.RailKeyCCBillFlexID: "abc-123", models.RailKeyCCBillRecurringBillingOption: "0000007498"},
	} {
		ids, err := a.Attach(context.Background(), link, autoCreateContext{})
		require.NoError(t, err)
		require.Equal(t, link, ids, "links are preserved exactly; nothing is invented")
	}
	for _, link := range []map[string]string{{models.RailKeyCCBillFormName: "premium"}, {}} {
		_, err := a.Attach(context.Background(), link, autoCreateContext{})
		require.Error(t, err, "%v", link)
	}
}

func TestResolveProviders(t *testing.T) {
	unconfigured := func() *Service { return &Service{rt: &app.Runtime{}} }
	product := &models.Product{ID: uuid.New(), Key: "premium"}
	recurring := func(psps ...string) CreatePriceRequest {
		return CreatePriceRequest{ProductID: openrails.ProductID(product.ID), UnitAmount: 23_000_000, Currency: "USD", AccessDurationHours: intPtr(720), AutoRenew: true, PSPs: psps}
	}

	t.Run("unknown provider fails loudly", func(t *testing.T) {
		_, _, _, err := unconfigured().resolveProviders(context.Background(), product, recurring("paypal"), uuid.New())
		require.ErrorContains(t, err, `unknown provider "paypal"`)
	})

	t.Run("engine terms request no provider mirrors", func(t *testing.T) {
		svc := &Service{rt: &app.Runtime{Config: &config.Config{}}}
		oneTime := recurring("stripe", "nmi")
		oneTime.AutoRenew, oneTime.AccessDurationHours = false, nil
		for _, req := range []CreatePriceRequest{recurring("stripe", "nmi"), oneTime} {
			links, states, pending, err := svc.resolveProviders(context.Background(), product, req, uuid.New())
			require.NoError(t, err)
			require.Empty(t, links)
			require.Empty(t, states)
			require.Empty(t, pending)
		}
		oneTime.PSPLinks = map[string]map[string]string{"stripe": {models.RailKeyStripePriceID: "price_legacy"}}
		links, _, _, err := svc.resolveProviders(context.Background(), product, oneTime, uuid.New())
		require.NoError(t, err)
		require.Equal(t, "price_legacy", links["stripe"][models.RailKeyStripePriceID], "an explicit legacy reference is kept")
	})

	t.Run("linked ccbill beside engine nmi", func(t *testing.T) {
		req := recurring("ccbill", "nmi")
		req.AutoRenew, req.AccessDurationHours = false, nil
		req.PSPLinks = map[string]map[string]string{"ccbill": {"form_name": "premium", "flex_id": "abc-123"}}
		links, states, pending, err := unconfigured().resolveProviders(context.Background(), product, req, uuid.New())
		require.NoError(t, err)
		require.Equal(t, ProviderStatusLinked, states["ccbill"].Status)
		require.NotContains(t, states, "nmi")
		require.NotContains(t, links, "nmi")
		require.Empty(t, pending)
	})

	// #346: limited/readonly mode never calls a provider write; native catalog
	// slots defer to a manual link and the price still applies locally.
	t.Run("remote writes disabled defers", func(t *testing.T) {
		svc := &Service{rt: &app.Runtime{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeLimited}}}
		req := recurring("stripe", "nmi", "solana")
		req.AutoRenew, req.AccessDurationHours = false, nil
		links, states, pending, err := svc.resolveProviders(context.Background(), product, req, uuid.New())
		require.NoError(t, err)
		require.Empty(t, links)
		require.NotContains(t, states, "stripe")
		require.NotContains(t, states, "nmi")
		require.Equal(t, ProviderStatusPendingManualLink, states["solana"].Status)
		require.Equal(t, remoteWritesDisabledMessage, states["solana"].Message)
		require.Len(t, pending, 1)
	})

	// or#896: a trial on a rail with no first phase is refused, never
	// silently dropped (which charged full price immediately).
	t.Run("trial only on rails with a first phase", func(t *testing.T) {
		withTrial := func(psp string) CreatePriceRequest {
			req := recurring(psp)
			req.TrialUnitAmount, req.TrialDurationHours = int64Ptr(0), intPtr(7*24)
			return req
		}
		for _, psp := range []string{"nmi", "solana"} {
			_, _, _, err := unconfigured().resolveProviders(context.Background(), product, withTrial(psp), uuid.New())
			require.ErrorIs(t, err, ErrTrialUnsupportedOnRail)
			require.ErrorContains(t, err, psp)
			require.ErrorContains(t, err, "silently dropped")
			_, _, _, err = unconfigured().resolveProviders(context.Background(), product, recurring(psp), uuid.New())
			require.NoError(t, err, psp)
		}
		for _, psp := range []string{"stripe", "ccbill"} {
			_, _, _, err := unconfigured().resolveProviders(context.Background(), product, withTrial(psp), uuid.New())
			require.NoError(t, err, psp)
		}
	})
}
