package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solsubs "github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

type fakeNMIResolver struct {
	client *nmi.NMIClient
	err    error
}

func (f fakeNMIResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return f.client, f.client != nil, f.err
}

func testNMIClient(t *testing.T, url string, readOnly bool) *nmi.NMIClient {
	t.Helper()
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "test_security_key", WebhookSecret: "test_secret"}, true)
	require.NoError(t, err)
	client.LoopbackFixture, client.ReadOnly = true, readOnly
	if url != "" {
		client.DirectPostURL, client.QueryURL, client.V5BaseURL = url, url, url
	}
	return client
}

func refundIntent(t *testing.T, typ string, mutate func(*RefundPayload)) gen.OpenrailsRailIntent {
	p := RefundPayload{Currency: "USD", OriginalPaymentID: uuid.New(), ReservationID: uuid.New(), AmountCents: 500, ProviderTarget: "txn_original"}
	if mutate != nil {
		mutate(&p)
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return gen.OpenrailsRailIntent{ID: uuid.New(), IntentType: typ, Rail: "nmi", Payload: raw, IdempotencyKey: "the-intent-key", Origin: string(OriginAdmin), Attempts: 1, Status: StatusInFlight}
}

func TestNMIHandlersParkBeforeProviderTraffic(t *testing.T) {
	sub, psp := uuid.New(), uuid.New()
	deleteIntent := gen.OpenrailsRailIntent{ID: uuid.New(), IntentType: TypeNMIDeleteSubscription, Rail: "nmi", SubscriptionID: &sub, PspID: &psp,
		Payload: []byte(`{"rail_subscription_id":"sub-target"}`), Origin: string(OriginUser), Attempts: 1, Status: StatusInFlight}
	unaddressed := deleteIntent
	unaddressed.PspID = nil
	readOnly := fakeNMIResolver{client: testNMIClient(t, "", true)}
	for _, tc := range []struct {
		name   string
		h      Handler
		intent gen.OpenrailsRailIntent
		reason string
	}{
		{"delete: unarmed", NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil), deleteIntent, "not armed"},
		{"delete: resolver error fails closed", NewNMIDeleteHandler(nil, nil, fakeNMIResolver{err: errors.New("vault down")}, nil), deleteIntent, "fail closed"},
		{"delete: no resolver", NewNMIDeleteHandler(nil, nil, nil, nil), deleteIntent, "fail closed"},
		{"delete: read-only client", NewNMIDeleteHandler(nil, nil, readOnly, nil), deleteIntent, "read-only"},
		{"delete: unaccepted target", NewNMIDeleteHandler(nil, nil, readOnly, nil), unaddressed, "no accepted subscription/provider address"},
		{"refund: unarmed", NewNMIRefundHandler(nil, fakeNMIResolver{}, nil), refundIntent(t, TypeNMIRefund, nil), "not armed"},
		{"refund: read-only client", NewNMIRefundHandler(nil, readOnly, nil), refundIntent(t, TypeNMIRefund, nil), "read-only"},
	} {
		out := tc.h.Execute(context.Background(), tc.intent)
		assert.Equal(t, OutcomeParked, out.Class, tc.name)
		assert.Contains(t, out.Reason, tc.reason, tc.name)
	}
	assert.Equal(t, OutcomeAmbiguous, NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil).Verify(context.Background(), deleteIntent).Class,
		"cannot verify: stay unknown")
}

// A money mover never blind-retries: a transport failure after the send is ambiguous.
func TestNMIRefundOutcomeClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(srv.Close)
	h := NewNMIRefundHandler(nil, fakeNMIResolver{client: testNMIClient(t, srv.URL, false)}, nil)
	out := h.Execute(context.Background(), refundIntent(t, TypeNMIRefund, nil))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
	assert.Contains(t, out.Reason, "outcome unknown")

	bad := refundIntent(t, TypeNMIRefund, nil)
	bad.Payload = []byte(`{"amount_cents": 0}`)
	assert.Equal(t, OutcomeTerminal, h.Execute(context.Background(), bad).Class)
}

// NMI delete's pre-write read: 200 active = present; 200 inactive tombstone or 404 = gone.
func TestNMISubscriptionPresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		present bool
		wantErr bool
	}{
		{"present", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"active"}`, true, false},
		{"inactive tombstone", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"inactive"}`, false, false},
		{"not found", http.StatusNotFound, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`, false, false},
		{"auth error", http.StatusUnauthorized, `{"type":"authenticationError","error_code":"E_AUTH","message":"invalid security key"}`, false, true},
		{"garbage", http.StatusOK, `not json at all`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/subscriptions/sub-1", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			present, err := (&NMIDeleteHandler{}).subscriptionPresent(context.Background(), testNMIClient(t, srv.URL, false), "sub-1")
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.present, present)
		})
	}
}

type fakeStripeRefunds struct {
	result    *subscriptions.RefundResult
	createErr error
	found     bool
	findErr   error
	gotParams subscriptions.RefundParams
}

func (f *fakeStripeRefunds) CreateRefund(_ context.Context, p subscriptions.RefundParams) (*subscriptions.RefundResult, error) {
	f.gotParams = p
	return f.result, f.createErr
}
func (f *fakeStripeRefunds) FindRefundByIdempotencyKey(context.Context, string, string) (*subscriptions.RefundResult, bool, error) {
	return f.result, f.found, f.findErr
}

// IDEM-5/IDEM-6: Stripe refunds carry the intent key and classify so that
// only a provably-unexecuted refund is retried.
func TestStripeRefundClassification(t *testing.T) {
	rails := railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}}
	apiErr := func(code int) error { return &subscriptions.StripeAPICallError{StatusCode: code, Message: "x"} }
	pending := &subscriptions.RefundResult{ID: "re_pending", Status: "pending"}
	intent := refundIntent(t, TypeStripeRefund, func(p *RefundPayload) { p.ProviderTarget = "ch_1" })

	unconfigured := NewStripeRefundHandler(nil, &config.Config{}, nil, nil, nil).Execute(context.Background(), intent)
	assert.Equal(t, OutcomeParked, unconfigured.Class)
	assert.Contains(t, unconfigured.Reason, "stripe not configured")

	for _, tc := range []struct {
		name   string
		fake   fakeStripeRefunds
		verify bool
		want   OutcomeClass
	}{
		{"rate limit is a clean retry", fakeStripeRefunds{createErr: apiErr(429)}, false, OutcomeRetryable},
		{"5xx may have created it", fakeStripeRefunds{createErr: apiErr(500)}, false, OutcomeAmbiguous},
		{"transport error may have created it", fakeStripeRefunds{createErr: errors.New("reset")}, false, OutcomeAmbiguous},
		{"readonly choke parks", fakeStripeRefunds{createErr: fmt.Errorf("post: %w", stripeapi.ErrProviderReadOnly)}, false, OutcomeParked},
		{"pending is not completion", fakeStripeRefunds{result: pending}, false, OutcomeAmbiguous},
		{"verify: clean miss proves not executed", fakeStripeRefunds{}, true, OutcomeRetryable},
		{"verify: read failure stays unknown", fakeStripeRefunds{findErr: errors.New("down")}, true, OutcomeAmbiguous},
		{"verify: pending is not completion", fakeStripeRefunds{result: pending, found: true}, true, OutcomeAmbiguous},
	} {
		h := NewStripeRefundHandler(nil, &config.Config{}, rails, nil, nil)
		fake := tc.fake
		h.Stripe = &fake
		var out Outcome
		if tc.verify {
			out = h.Verify(context.Background(), intent)
		} else {
			out = h.Execute(context.Background(), intent)
			assert.Equal(t, "the-intent-key", fake.gotParams.IdempotencyKey, tc.name)
			assert.Equal(t, moneyutil.Cents(500), fake.gotParams.Amount, "%s: payload cents reach Stripe verbatim (#671)", tc.name)
			assert.Equal(t, "ch_1", fake.gotParams.ChargeID, tc.name)
		}
		assert.Equal(t, tc.want, out.Class, tc.name)
	}
}

func TestCCBillRefundAlwaysRequiresOperatorVerification(t *testing.T) {
	h := NewCCBillRefundHandler(nil, nil)
	for _, attempts := range []int32{1, 2, 4} {
		row := refundIntent(t, TypeCCBillRefund, func(p *RefundPayload) { p.ProviderTarget, p.ProviderTransactionID = "sub_123", "requested_charge" })
		row.Attempts = attempts
		for _, out := range []Outcome{h.Execute(context.Background(), row), h.Verify(context.Background(), row)} {
			assert.Equal(t, OutcomeAmbiguous, out.Class)
			assert.Contains(t, out.Reason, "operator must verify")
		}
	}
	keepPayload, keepEvidence := h.PrunePolicy()
	assert.True(t, keepPayload && keepEvidence)
}

// fakeStripeCatalog: objects maps id -> active; a missing id is a 404.
type fakeStripeCatalog struct {
	objects           map[string]bool
	readErr, writeErr error
	writes            map[string]string // id -> idempotency key of the active=false write
}

func (f *fakeStripeCatalog) find(id string) (bool, bool, error) {
	active, ok := f.objects[id]
	return active, ok, f.readErr
}
func (f *fakeStripeCatalog) FindProduct(_ context.Context, id string) (*catalog.StripeProduct, bool, error) {
	active, ok, err := f.find(id)
	if err != nil || !ok {
		return nil, false, err
	}
	return &catalog.StripeProduct{ID: id, Active: active}, true, nil
}
func (f *fakeStripeCatalog) FindPrice(_ context.Context, id string) (*catalog.StripePrice, bool, error) {
	active, ok, err := f.find(id)
	if err != nil || !ok {
		return nil, false, err
	}
	return &catalog.StripePrice{ID: id, Active: active}, true, nil
}
func (f *fakeStripeCatalog) write(id string, active *bool, key string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if active == nil || *active {
		return errors.New("archive must write active=false")
	}
	f.writes[id], f.objects[id] = key, false
	return nil
}
func (f *fakeStripeCatalog) UpdateProduct(_ context.Context, id string, p catalog.UpdateProductParams) error {
	return f.write(id, p.Active, p.IdempotencyKey)
}
func (f *fakeStripeCatalog) UpdatePrice(_ context.Context, id string, p catalog.UpdatePriceParams) error {
	return f.write(id, p.Active, p.IdempotencyKey)
}

func stubCatalog(products []*models.Product, prices []*models.Price) catalogRowsLoader {
	return func(context.Context) ([]*models.Product, []*models.Price, error) { return products, prices, nil }
}

func archiveHandler(typ string, api stripeCatalogAPI, loader catalogRowsLoader) Handler {
	core := stripeArchiveCore{Stripe: api, LoadCatalog: loader, Policy: DefaultBackoff}
	if typ == TypeStripeArchiveProduct {
		return &StripeArchiveProductHandler{core}
	}
	return &StripeArchivePriceHandler{core}
}

func archiveIntent(t *testing.T, typ string, psp uuid.UUID, objectID, marker string) gen.OpenrailsRailIntent {
	payload, err := json.Marshal(StripeArchivePayload{ObjectID: objectID, MarkerKey: marker})
	require.NoError(t, err)
	return gen.OpenrailsRailIntent{ID: uuid.New(), PspID: &psp, Rail: "stripe", IntentType: typ, Payload: payload,
		IdempotencyKey: StripeArchiveIdempotencyKey(typ, objectID), Origin: string(OriginAdmin)}
}

// Archives verify-then-execute and are idempotent: a failed write is a clean retry, never ambiguous.
func TestStripeArchiveVerifyThenExecute(t *testing.T) {
	for _, typ := range []string{TypeStripeArchiveProduct, TypeStripeArchivePrice} {
		for _, tc := range []struct {
			name              string
			objects           map[string]bool
			readErr, writeErr error
			unconfigured      bool
			exec, verify      OutcomeClass
			evidence          string
			wrote             bool
		}{
			{name: "active is archived", objects: map[string]bool{"obj": true}, exec: OutcomeSucceeded, verify: OutcomeRetryable, evidence: "archived", wrote: true},
			{name: "absent", objects: map[string]bool{}, exec: OutcomeSucceeded, verify: OutcomeSucceeded, evidence: "verified_absent"},
			{name: "already inactive", objects: map[string]bool{"obj": false}, exec: OutcomeSucceeded, verify: OutcomeSucceeded, evidence: "already_inactive"},
			{name: "readonly choke", objects: map[string]bool{"obj": true}, writeErr: fmt.Errorf("post: %w", stripeapi.ErrProviderReadOnly), exec: OutcomeParked, verify: OutcomeRetryable},
			{name: "write failure", objects: map[string]bool{"obj": true}, writeErr: errors.New("stripe 500"), exec: OutcomeRetryable, verify: OutcomeRetryable},
			{name: "read failure", objects: map[string]bool{"obj": true}, readErr: errors.New("timeout"), exec: OutcomeRetryable, verify: OutcomeAmbiguous},
			{name: "unconfigured", unconfigured: true, exec: OutcomeParked, verify: OutcomeAmbiguous},
		} {
			name := typ + "/" + tc.name
			api := &fakeStripeCatalog{objects: tc.objects, readErr: tc.readErr, writeErr: tc.writeErr, writes: map[string]string{}}
			var client stripeCatalogAPI = api
			if tc.unconfigured {
				client = nil
			}
			intent := archiveIntent(t, typ, uuid.New(), "obj", "k")
			// Verify first: it must be read-only whatever the state.
			assert.Equal(t, tc.verify, archiveHandler(typ, client, stubCatalog(nil, nil)).Verify(context.Background(), intent).Class, name)
			assert.Empty(t, api.writes, name)
			out := archiveHandler(typ, client, stubCatalog(nil, nil)).Execute(context.Background(), intent)
			assert.Equal(t, tc.exec, out.Class, "%s: %s", name, out.Reason)
			if tc.evidence != "" {
				assert.Equal(t, true, out.Evidence[tc.evidence], name)
			}
			if tc.wrote {
				assert.Equal(t, map[string]string{"obj": intent.IdempotencyKey}, api.writes, "%s: the write carries the intent key", name)
				assert.Equal(t, OutcomeSucceeded, archiveHandler(typ, client, stubCatalog(nil, nil)).Verify(context.Background(), intent).Class, name)
			} else {
				assert.Empty(t, api.writes, name)
			}
		}
	}
}

// The archive applies only while the object is still an extra (catalog.ExtrasIndex).
func TestStripeArchiveSupersededOnceObjectJoinsCatalog(t *testing.T) {
	psp, productID, cycle := uuid.New(), uuid.New(), 720
	price := func(links map[string]map[string]string) *models.Price {
		return &models.Price{ID: uuid.New(), ProductID: productID, Amount: 900, Currency: "USD", AccessDurationHours: &cycle, AutoRenew: true, PSPLinks: links}
	}
	product := &models.Product{ID: productID, Key: "retired"}
	linked := map[string]map[string]string{"stripe": {models.RailKeyPSPID: psp.String(), models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_x"}}
	for _, tc := range []struct {
		name, typ, object, marker string
		loader                    catalogRowsLoader
	}{
		{"product content key", TypeStripeArchiveProduct, "prod_x", "retired", stubCatalog([]*models.Product{product}, nil)},
		{"price linked by id", TypeStripeArchivePrice, "price_x", "retired.usd.900.30", stubCatalog(nil, []*models.Price{price(linked)})},
		{"price content key", TypeStripeArchivePrice, "price_x", "retired.usd.900.30", stubCatalog([]*models.Product{product}, []*models.Price{price(nil)})},
	} {
		intent := archiveIntent(t, tc.typ, psp, tc.object, tc.marker)
		rel, err := archiveHandler(tc.typ, nil, stubCatalog(nil, nil)).CheckRelevance(context.Background(), intent)
		require.NoError(t, err, tc.name)
		assert.True(t, rel.Applicable, "%s: still an extra", tc.name)
		rel, err = archiveHandler(tc.typ, nil, tc.loader).CheckRelevance(context.Background(), intent)
		require.NoError(t, err, tc.name)
		assert.False(t, rel.Applicable, "%s: joined the catalog", tc.name)
	}

	rel, err := archiveHandler(TypeStripeArchiveProduct, nil, stubCatalog(nil, nil)).CheckRelevance(context.Background(),
		gen.OpenrailsRailIntent{IntentType: TypeStripeArchiveProduct, Payload: []byte(`{}`)})
	require.NoError(t, err)
	assert.False(t, rel.Applicable, "malformed payload supersedes (surfaces in reconcile) instead of parking forever")
	unaddressed := archiveIntent(t, TypeStripeArchiveProduct, psp, "prod_x", "k")
	unaddressed.PspID = nil
	_, err = archiveHandler(TypeStripeArchiveProduct, nil, stubCatalog(nil, nil)).CheckRelevance(context.Background(), unaddressed)
	assert.Error(t, err, "an intent without its PSP cannot be judged; keep it pending")
}

type fakePlanReader struct {
	accounts map[string][]byte
	err      error
}

func (f *fakePlanReader) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	return f.accounts[address.String()], f.err
}

type fakeSolanaSubmitter struct {
	merchant  solanago.PublicKey
	submitted []solanago.Instruction
	err       error
}

func (f *fakeSolanaSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return f.merchant, nil
}
func (f *fakeSolanaSubmitter) Submit(_ context.Context, _ merchant.ID, ixs []solanago.Instruction) (solanago.Signature, error) {
	if f.err != nil {
		return solanago.Signature{}, f.err
	}
	f.submitted = append(f.submitted, ixs...)
	return solanago.Signature{}, nil
}

func planAccount(owner solanago.PublicKey, status uint8) []byte {
	blob := make([]byte, solsubs.PlanAccountSize)
	blob[0] = 1 // plan discriminator
	copy(blob[1:33], owner.Bytes())
	blob[33] = 1 // bump
	blob[34] = status
	return blob
}

func sunsetIntent(t *testing.T, psp uuid.UUID, pda string) gen.OpenrailsRailIntent {
	payload, err := json.Marshal(SolanaSunsetPayload{PlanPDA: pda})
	require.NoError(t, err)
	return gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: uuid.New(), Rail: "solana", PspID: &psp, IntentType: TypeSolanaSunsetPlan,
		Payload: payload, IdempotencyKey: SolanaSunsetIdempotencyKey(pda), Origin: string(OriginAdmin)}
}

func TestSolanaSunsetVerifyThenExecute(t *testing.T) {
	owner, foreign := solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey()
	pda := solanago.NewWallet().PublicKey().String()
	for _, tc := range []struct {
		name         string
		account      []byte
		readErr      error
		submitErr    error
		unconfigured bool
		exec, verify OutcomeClass
		evidence     string
		submitted    bool
	}{
		{name: "active is sunset", account: planAccount(owner, solsubs.PlanStatusActive), exec: OutcomeSucceeded, verify: OutcomeRetryable, evidence: "sunset", submitted: true},
		{name: "already sunset", account: planAccount(owner, solsubs.PlanStatusSunset), exec: OutcomeSucceeded, verify: OutcomeSucceeded, evidence: "already_sunset"},
		{name: "absent", exec: OutcomeSucceeded, verify: OutcomeSucceeded, evidence: "verified_absent"},
		{name: "foreign owner can never succeed", account: planAccount(foreign, solsubs.PlanStatusActive), exec: OutcomeTerminal, verify: OutcomeRetryable},
		{name: "submit failure may have landed", account: planAccount(owner, solsubs.PlanStatusActive), submitErr: errors.New("rpc timeout mid-send"), exec: OutcomeAmbiguous, verify: OutcomeRetryable},
		{name: "readonly choke", account: planAccount(owner, solsubs.PlanStatusActive), submitErr: solanaint.ErrProviderReadOnly, exec: OutcomeParked, verify: OutcomeRetryable},
		{name: "read failure", readErr: errors.New("rpc down"), exec: OutcomeRetryable, verify: OutcomeAmbiguous},
		{name: "unconfigured", unconfigured: true, exec: OutcomeParked, verify: OutcomeAmbiguous},
	} {
		reader := &fakePlanReader{accounts: map[string][]byte{}, err: tc.readErr}
		if tc.account != nil {
			reader.accounts[pda] = tc.account
		}
		submitter := &fakeSolanaSubmitter{merchant: owner, err: tc.submitErr}
		h := &SolanaSunsetPlanHandler{LoadCatalog: stubCatalog(nil, nil), Policy: DefaultBackoff}
		if !tc.unconfigured {
			h.Reader, h.Plans = reader, recurring.NewPlanServiceWithReader(submitter, nil, "devnet")
		}
		intent := sunsetIntent(t, uuid.New(), pda)
		assert.Equal(t, tc.verify, h.Verify(context.Background(), intent).Class, tc.name)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, tc.exec, out.Class, "%s: %s", tc.name, out.Reason)
		if tc.evidence != "" {
			assert.Equal(t, true, out.Evidence[tc.evidence], tc.name)
		}
		if !tc.submitted {
			assert.Empty(t, submitter.submitted, tc.name)
			continue
		}
		require.Len(t, submitter.submitted, 1, tc.name)
		data, err := submitter.submitted[0].Data()
		require.NoError(t, err)
		assert.Equal(t, []byte{8, solsubs.PlanStatusSunset}, data[:2], "update_plan(status=sunset)")
	}
}

func TestSolanaSunsetSupersededWhenPlanRejoinsCatalog(t *testing.T) {
	psp, cycle := uuid.New(), 720
	pda := solanago.NewWallet().PublicKey().String()
	price := func(archived bool) *models.Price {
		return &models.Price{ID: uuid.New(), ProductID: uuid.New(), Amount: 2300, Currency: "USD", AccessDurationHours: &cycle, AutoRenew: true, Archived: archived,
			PSPLinks: map[string]map[string]string{string(models.RailSolana): {models.RailKeyPSPID: psp.String(), models.RailKeyRail: "solana", "plan_pda": pda}}}
	}
	for archived, applicable := range map[bool]bool{true: true, false: false} {
		h := &SolanaSunsetPlanHandler{LoadCatalog: stubCatalog(nil, []*models.Price{price(archived)}), Policy: DefaultBackoff}
		rel, err := h.CheckRelevance(context.Background(), sunsetIntent(t, psp, pda))
		require.NoError(t, err)
		assert.Equal(t, applicable, rel.Applicable, "archived=%v", archived)
	}
}
