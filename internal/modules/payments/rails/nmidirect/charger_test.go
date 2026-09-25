package nmidirect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// fakeGateway is NMI at its HTTP boundary: v5 vault reads on GET, direct-post sales on POST.
type fakeGateway struct {
	mu      sync.Mutex
	billing string // JSON billing array for GET /customers/v1
	sale    string // direct-post response body; "disconnect" drops the connection
	reads   int
	forms   []url.Values
}

func (g *fakeGateway) Forms() []url.Values {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]url.Values(nil), g.forms...)
}

func (g *fakeGateway) Reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

func newCharger(t *testing.T, g *fakeGateway) *Charger {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Method == http.MethodGet {
			g.reads++
			require.Equal(t, "/customers/v1", r.URL.Path)
			require.Equal(t, "account-key", r.Header.Get("Authorization"))
			fmt.Fprintf(w, `{"object":"customer","id":"v1","billing":%s}`, g.billing)
			return
		}
		require.NoError(t, r.ParseForm())
		g.forms = append(g.forms, r.PostForm)
		if g.sale == "disconnect" {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			return
		}
		fmt.Fprint(w, g.sale)
	}))
	t.Cleanup(server.Close)
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "account-key"}, true)
	require.NoError(t, err)
	client.LoopbackFixture = true
	client.DirectPostURL, client.V5BaseURL, client.QueryURL = server.URL+"/sale", server.URL, server.URL+"/query"
	return New(client)
}

func request(ctx charge.Context) charge.Request {
	return charge.Request{
		Instrument:  charge.Instrument{Rail: "nmi", CustomerRef: "v1", MethodRef: "b1"},
		AmountMinor: moneyutil.Cents(500), Currency: "USD", Description: "test charge", OrderRef: "order-1", Context: ctx,
	}
}

func TestStoredCredentialFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ctx   charge.Context
		want  nmi.StoredCredential
		valid bool
	}{
		{charge.InitialOneTime(), nmi.StoredCredential{InitiatedBy: "customer", Indicator: "stored"}, true},
		{charge.OneTimeReuse("ref-1"), nmi.StoredCredential{InitiatedBy: "customer", Indicator: "used", InitialTransactionID: "ref-1"}, true},
		{charge.InitialRecurring(), nmi.StoredCredential{InitiatedBy: "customer", Indicator: "stored", Recurring: true}, true},
		{charge.RecurringReuse("ref-2"), nmi.StoredCredential{InitiatedBy: "customer", Indicator: "used", InitialTransactionID: "ref-2", Recurring: true}, true},
		{charge.RecurringMIT(" ref-3 "), nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used", InitialTransactionID: "ref-3", Recurring: true}, true},
		{charge.UnscheduledMIT("ref-4"), nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used", InitialTransactionID: "ref-4"}, true},
		{charge.UnscheduledMIT(""), nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used"}, false},
	} {
		got := StoredCredentialFor(tc.ctx)
		require.Equal(t, tc.want, *got, "%+v", tc.ctx)
		require.Equal(t, tc.valid, got.Validate() == nil, "%+v", tc.ctx)
	}
}

func TestFailureCodeAndHardDecline(t *testing.T) {
	t.Parallel()
	require.Equal(t, "insufficient_funds", FailureCode(&nmi.CustomerVaultError{ResponseCode: 202, LocalizationID: " insufficient_funds "}))
	require.Equal(t, "nmi_response_202", FailureCode(&nmi.CustomerVaultError{ResponseCode: 202}))
	require.Equal(t, "nmi_declined", FailureCode(&nmi.CustomerVaultError{}))
	require.Equal(t, "nmi_declined", FailureCode(nil))
	for code, hard := range map[int]bool{200: true, 202: true, 300: true, 420: false, 421: false, 430: false} {
		require.Equal(t, hard, IsHardDecline(code), code)
	}
}

func TestChargeCapturesAnchorAndReplaysIt(t *testing.T) {
	t.Parallel()
	g := &fakeGateway{sale: "response=1&transactionid=txn-99&authcode=OK&response_code=100"}
	c := newCharger(t, g)

	res, err := c.Charge(context.Background(), request(charge.InitialOneTime()))
	require.NoError(t, err)
	require.Equal(t, charge.Result{TransactionID: "txn-99", TokenType: charge.TokenTypePSPToken, CapturedRef: "txn-99"}, res)
	require.Equal(t, "customer", g.Forms()[0].Get("initiated_by"))
	require.Equal(t, "stored", g.Forms()[0].Get("stored_credential_indicator"))

	res, err = c.Charge(context.Background(), request(charge.UnscheduledMIT("anchor-1")))
	require.NoError(t, err)
	require.Empty(t, res.CapturedRef, "only the initial CIT anchors the sequence")
	require.Equal(t, "anchor-1", g.Forms()[1].Get("initial_transaction_id"))
	require.Equal(t, "merchant", g.Forms()[1].Get("initiated_by"))

	for name, req := range map[string]charge.Request{
		"reference-less MIT": request(charge.UnscheduledMIT("")),
		"no vault":           func() charge.Request { r := request(charge.InitialOneTime()); r.Instrument.CustomerRef = " "; return r }(),
		"zero amount":        func() charge.Request { r := request(charge.InitialOneTime()); r.AmountMinor = 0; return r }(),
	} {
		_, err := c.Charge(context.Background(), req)
		require.Error(t, err, name)
	}
	require.Len(t, g.Forms(), 2, "invalid requests never reach NMI")
}

func TestChargeOutcomeClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		body     string
		declined bool
		code     string
	}{
		{"response=2&responsetext=DECLINED&response_code=202", true, "insufficient_funds"},
		{"response=2&responsetext=Duplicate&response_code=430", false, ""},
		{"response=3&response_code=400&responsetext=RAW_PROVIDER_SENTINEL", false, ""},
		{"response=2&response=1&response_code=202&response_code=100", false, ""},
		{"response=2&response_code=100", false, ""},
		{"response=2", false, ""},
	} {
		g := &fakeGateway{sale: tc.body}
		res, err := newCharger(t, g).Charge(context.Background(), request(charge.UnscheduledMIT("anchor-1")))
		require.Len(t, g.Forms(), 1, tc.body)
		if tc.declined {
			require.NoError(t, err, tc.body)
			require.True(t, res.Declined)
			require.Equal(t, tc.code, *res.FailureCode)
			continue
		}
		require.Error(t, err, tc.body)
		require.False(t, res.Declined, "an unqualified response never becomes a decline: %s", tc.body)
		require.NotContains(t, err.Error(), "RAW_PROVIDER_SENTINEL")
	}
}

// Engine recurring: a plain sale on the exact vault entry, never an NMI schedule.
func TestRecurringSaleWire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ctx      charge.Context
		initial  bool
		captured string
	}{
		{charge.InitialRecurring(), true, "txn"},
		{charge.RecurringReuse("anchor"), true, ""},
		{charge.RecurringMIT("anchor"), false, ""},
	} {
		g := &fakeGateway{billing: `[{"id":"b1"}]`, sale: "response=1&response_code=100&transactionid=txn"}
		c := newCharger(t, g)
		run := c.ChargeRecurringMIT
		if tc.initial {
			run = c.ChargeInitialRecurring
		}
		res, refusal, err := run(context.Background(), request(tc.ctx))
		require.NoError(t, err)
		require.Nil(t, refusal)
		require.Equal(t, 1, g.Reads(), "the vault entry is verified before money moves")
		require.Equal(t, "txn", res.TransactionID)
		require.Equal(t, tc.captured, res.CapturedRef)
		w := g.Forms()[0]
		for field, want := range map[string]string{
			"type": "sale", "recurring": "", "subscription_id": "", "plan_id": "", "billing_method": "recurring",
			"initiated_by": string(tc.ctx.Initiator), "initial_transaction_id": tc.ctx.PriorRef,
			"customer_vault_id": "v1", "billing_id": "b1", "security_key": "account-key", "amount": "5.00", "currency": "USD", "orderid": "order-1",
		} {
			require.Equal(t, want, w.Get(field), field)
		}
	}
}

func TestRecurringInvalidTermsNeverDispatch(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*charge.Request, *Charger){
		"missing anchor":      func(r *charge.Request, _ *Charger) { r.Context = charge.RecurringMIT("") },
		"unscheduled":         func(r *charge.Request, _ *Charger) { r.Context = charge.UnscheduledMIT("anchor") },
		"customer initiated":  func(r *charge.Request, _ *Charger) { r.Context = charge.RecurringReuse("anchor") },
		"wrong rail":          func(r *charge.Request, _ *Charger) { r.Instrument.Rail = "stripe" },
		"empty billing":       func(r *charge.Request, _ *Charger) { r.Instrument.MethodRef = "" },
		"whitespace vault":    func(r *charge.Request, _ *Charger) { r.Instrument.CustomerRef = " v1" },
		"unknown currency":    func(r *charge.Request, _ *Charger) { r.Currency = "??" },
		"zero amount":         func(r *charge.Request, _ *Charger) { r.AmountMinor = 0 },
		"missing operation":   func(r *charge.Request, _ *Charger) { r.OrderRef = "" },
		"changed credentials": func(_ *charge.Request, c *Charger) { c.Client.SecurityKey = "another-account" },
		"read only":           func(_ *charge.Request, c *Charger) { c.Client.ReadOnly = true },
	} {
		g := &fakeGateway{billing: `[{"id":"b1"}]`}
		c := newCharger(t, g)
		req := request(charge.RecurringMIT("anchor"))
		change(&req, c)
		_, refusal, err := c.ChargeRecurringMIT(context.Background(), req)
		require.ErrorIs(t, err, charge.ErrNotDispatched, name)
		require.Nil(t, refusal, name)
		require.Empty(t, g.Forms(), name)
	}
	for _, billing := range []string{`[]`, `[{"id":"other"}]`, `[{"id":"b1"},{"id":"b2"}]`} {
		g := &fakeGateway{billing: billing}
		_, _, err := newCharger(t, g).ChargeRecurringMIT(context.Background(), request(charge.RecurringMIT("anchor")))
		require.ErrorIs(t, err, charge.ErrNotDispatched, billing)
		require.Equal(t, 1, g.Reads(), billing)
		require.Empty(t, g.Forms(), "a vault mismatch never sends money: %s", billing)
	}
}

// After dispatch, only a clean 2xx refusal is a decline; everything else is unknown and never retried.
func TestRecurringRefusalVersusUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, response string
		hard           bool
	}{
		{"decline", "response=2&response_code=202&responsetext=secret", true},
		{"communication", "response=3&response_code=420", false},
		{"duplicate code without refusal text", "response=3&response_code=430", false},
		{"unqualified gateway error", "response=3&response_code=400", false},
		{"contradictory decline", "response=3&response_code=202", false},
		{"approval missing id", "response=1&response_code=100", false},
		{"malformed", "bad payload", false},
		{"lost response", "disconnect", false},
	} {
		g := &fakeGateway{billing: `[{"id":"b1"}]`, sale: tc.response}
		res, refusal, err := newCharger(t, g).ChargeRecurringMIT(context.Background(), request(charge.RecurringMIT("anchor")))
		require.Len(t, g.Forms(), 1, tc.name)
		if tc.hard {
			require.NoError(t, err, tc.name)
			require.True(t, res.Declined)
			require.Equal(t, 202, refusal.ResponseCode)
			require.Equal(t, "response=2&response_code=202", refusal.RawResponse, "gateway free text never escapes")
			require.NotContains(t, refusal.Error(), "secret")
			continue
		}
		require.Error(t, err, tc.name)
		require.NotErrorIs(t, err, charge.ErrNotDispatched, tc.name)
		require.Nil(t, refusal, tc.name)
		require.False(t, res.Declined, tc.name)
	}

	// NMI's duplicate-check refusal: the matching charge may have paid this
	// period, so a renewal stays unknown; a customer-present request did not execute.
	dup := "response=3&response_code=300&responsetext=Duplicate+transaction+REFID:1"
	g := &fakeGateway{billing: `[{"id":"b1"}]`, sale: dup}
	_, _, err := newCharger(t, g).ChargeRecurringMIT(context.Background(), request(charge.RecurringMIT("anchor")))
	require.Error(t, err)
	require.NotErrorIs(t, err, charge.ErrNotDispatched, "a duplicate renewal is unknown, never released")
	g = &fakeGateway{billing: `[{"id":"b1"}]`, sale: dup}
	_, _, err = newCharger(t, g).ChargeInitialRecurring(context.Background(), request(charge.RecurringReuse("anchor")))
	require.ErrorIs(t, err, charge.ErrNotDispatched)
}
