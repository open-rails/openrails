//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// This loopback vendor proves engine custody/replay, not browser tokenization.
// The pinned HyperSwitch SDK/vault browser proof is a separate required gate.
type captureFixtureSession struct {
	ID, Customer, Secret, Token string
	Expiry                      time.Time
	Ready                       bool
}
type captureFixture struct {
	mu              sync.Mutex
	server          *httptest.Server
	account         string
	users           map[string]string
	sessions        map[string]*captureFixtureSession
	barrier         chan struct{}
	arrived         int
	preflight       string
	afterMethodRead func()
}

func newCaptureFixture(t *testing.T) *captureFixture {
	g := &captureFixture{account: "capture-" + uuid.NewString(), users: map[string]string{}, sessions: map[string]*captureFixtureSession{}}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "api-key=capture-fixture-key", r.Header.Get("Authorization"))
		require.Equal(t, "capture-profile", r.Header.Get("x-profile-id"))
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) { require.NoError(t, json.NewEncoder(w).Encode(value)) }
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v2/proxy":
			switch g.preflight {
			case "missing":
				w.WriteHeader(404)
			case "stock":
				write(map[string]any{"status": "ok"})
			default:
				write(map[string]any{"contract": "openrails-nmi-form-v1", "strict": g.preflight != "disabled", "max_response_bytes": 65536, "routes": []any{map[string]string{"destination_url": "https://secure.nmi.com/api/transact.php", "method": "POST", "response_profile": "nmi_classic"}}})
			}
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/customers/reference/"):
			ref := strings.TrimPrefix(r.URL.Path, "/v2/customers/reference/")
			id := g.users[ref]
			if id == "" {
				w.WriteHeader(404)
				return
			}
			write(map[string]string{"id": id, "merchant_reference_id": ref})
		case r.Method == "POST" && r.URL.Path == "/v2/customers":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			ref := body["merchant_reference_id"]
			if g.users[ref] == "" {
				g.users[ref] = "customer-" + ref
			}
			write(map[string]string{"id": g.users[ref], "merchant_reference_id": ref})
		case r.Method == "POST" && r.URL.Path == "/v2/payment-method-sessions":
			var body struct {
				Customer string `json:"customer_id"`
				Storage  string `json:"storage_type"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "persistent", body.Storage)
			id := "capture-" + uuid.NewString()
			s := &captureFixtureSession{ID: id, Customer: body.Customer, Secret: "private-" + uuid.NewString(), Token: "token-" + uuid.NewString(), Expiry: time.Now().UTC().Add(10 * time.Minute)}
			g.sessions[id] = s
			if gate := g.barrier; gate != nil {
				g.arrived++
				if g.arrived == 2 {
					close(gate)
					g.barrier = nil
				}
				g.mu.Unlock()
				select {
				case <-gate:
				case <-r.Context().Done():
				}
				g.mu.Lock()
			}
			write(map[string]any{"id": id, "customer_id": s.Customer, "storage_type": "persistent", "expires_at": s.Expiry, "client_secret": "client-" + s.Secret, "sdk_authorization": s.Secret})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/payment-method-sessions/"):
			s := g.sessions[strings.TrimPrefix(r.URL.Path, "/v2/payment-method-sessions/")]
			if s == nil {
				w.WriteHeader(404)
				return
			}
			methods := []any{}
			if s.Ready {
				methods = append(methods, map[string]any{"payment_method_token": map[string]string{"type": "payment_method_session_token", "data": s.Token}})
			}
			write(map[string]any{"id": s.ID, "customer_id": s.Customer, "storage_type": "persistent", "expires_at": s.Expiry, "associated_payment_methods": methods})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/payment-methods/"):
			require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
			require.Equal(t, "false", r.URL.Query().Get("force_sync"))
			token := strings.TrimPrefix(r.URL.Path, "/v2/payment-methods/")
			for _, s := range g.sessions {
				if s.Token == token && s.Ready {
					if hook := g.afterMethodRead; hook != nil {
						g.mu.Unlock()
						hook()
						g.mu.Lock()
					}
					write(map[string]any{"id": "method-" + s.ID, "merchant_id": g.account, "customer_id": s.Customer, "storage_type": "persistent", "payment_method_data": map[string]any{"card": map[string]string{"last4_digits": "4242", "expiry_month": "12", "expiry_year": "2030", "card_network": "Visa"}}})
					return
				}
			}
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected vendor request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}
func (g *captureFixture) complete(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	s.Ready = true
	return s.Token
}
func (g *captureFixture) count() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.sessions) }

type loseCaptureReply struct {
	transport http.RoundTripper
	t         *testing.T
}

func (l loseCaptureReply) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := l.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	require.Equal(l.t, "no-store", response.Header.Get("Cache-Control"))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, io.ErrUnexpectedEOF
}

func TestHyperSwitchCaptureSetupWorkflow(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCaptureFixture(t)
	configure := func(cfg *config.Config) {
		cfg.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: g.server.URL, SDKURL: g.server.URL + "/sdk.js"}
		cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}
	surface := h.StartStandalone("usd", WithConfig(configure))
	owned := surface.ProvisionOwnedMerchant("capture-" + uuid.NewString()[:8])
	remote := surface.Client(openrails.WithAPIKey(owned.APIKey))
	mid, custodian := owned.MerchantID, uuid.New()
	psp := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), mid.UUID(), "nmi")
	_, err := h.sharedPool().Exec(ctx, `INSERT INTO openrails.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"public_api_key":"capture-public","profile_id":"capture-profile"}','{"api_key":1}')`, custodian, mid.UUID(), "capture-"+custodian.String(), g.account)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.psps SET custodian_id=$1 WHERE merchant_id=$2 AND id=$3`, custodian, mid.UUID(), psp)
	require.NoError(t, err)
	name, err := merchants.CustodianSecretName("hyperswitch", "test", g.account, "api_key")
	require.NoError(t, err)
	_, err = surface.App().Runtime.Merchants.Secrets().Put(ctx, mid, name, "capture-fixture-key")
	require.NoError(t, err)
	newEmbedded := func(schema ...string) (*embed.Runtime, *openrails.Client) {
		cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}}
		configure(cfg)
		if len(schema) > 0 {
			cfg.DB.Schema = schema[0]
		}
		rt, e := embed.New(ctx, embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails()})
		require.NoError(t, e)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		client, e := rt.Client(openrails.WithMerchantID(mid))
		require.NoError(t, e)
		return rt, client
	}
	_, embedded := newEmbedded()
	request := func() openrails.CreateCheckoutSessionRequest {
		return openrails.CreateCheckoutSessionRequest{Mode: "payment_method", IdempotencyKey: uuid.NewString(), Customer: openrails.CheckoutCustomerIdentity{ID: openrails.CustomerID(uuid.New()), VerifiedEmail: "capture@example.test", Username: "capture"}, Payment: openrails.CheckoutPayment{PSPID: psp}}
	}
	for label, client := range map[string]*openrails.Client{"remote": remote, "embedded": embedded} {
		t.Run(label, func(t *testing.T) {
			req := request()
			created, err := client.CreateCheckoutSession(ctx, req)
			require.NoError(t, err)
			require.Equal(t, "requires_action", created.Status)
			require.NotNil(t, created.Capture)
			require.Nil(t, created.PriceID)
			require.Nil(t, created.Amount)
			require.Nil(t, created.Currency)
			_, err = surface.Client().GetCheckoutSession(ctx, req.Customer.ID, created.ID)
			require.Error(t, err, "another merchant must not read the action")
			var state string
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT rail_state::text FROM openrails.checkout_sessions WHERE merchant_id=$1 AND id=$2`, mid.UUID(), created.ID.UUID()).Scan(&state))
			require.NotContains(t, state, created.Capture.SDKAuthorization)
			require.Contains(t, state, "secret_ciphertext")
			before := g.count()
			again, err := client.CreateCheckoutSession(ctx, req)
			require.NoError(t, err)
			require.Equal(t, created.Capture, again.Capture)
			require.Equal(t, before, g.count())
			changed := req
			changed.Metadata = map[string]string{"changed": "body"}
			_, err = client.CreateCheckoutSession(ctx, changed)
			require.Error(t, err)
			require.Equal(t, before, g.count())
			_, err = client.GetCheckoutSession(ctx, openrails.CustomerID(uuid.New()), created.ID)
			require.Error(t, err)
			confirm := openrails.ConfirmCheckoutSessionRequest{CustomerID: req.Customer.ID, Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: custodian, SessionID: created.Capture.SessionID, Token: "not-completed"}}}
			_, err = client.ConfirmCheckoutSession(ctx, created.ID, confirm)
			require.Error(t, err)
			otherRequest := req
			otherRequest.IdempotencyKey = uuid.NewString()
			otherSession, err := client.CreateCheckoutSession(ctx, otherRequest)
			require.NoError(t, err)
			confirm.Payment.Capture.Token = g.complete(otherSession.Capture.SessionID)
			_, err = client.ConfirmCheckoutSession(ctx, created.ID, confirm)
			require.Error(t, err, "another session's completed token is not authority")
			confirm.Payment.Capture.Token = g.complete(created.Capture.SessionID)
			completed, err := client.ConfirmCheckoutSession(ctx, created.ID, confirm)
			require.NoError(t, err)
			require.Equal(t, "succeeded", completed.Status)
			require.NotNil(t, completed.PaymentMethodID)
			require.Nil(t, completed.Capture)
			replay, err := client.ConfirmCheckoutSession(ctx, created.ID, confirm)
			require.NoError(t, err)
			require.Equal(t, completed.PaymentMethodID, replay.PaymentMethodID)
			require.Nil(t, replay.Capture)
			get, err := client.GetCheckoutSession(ctx, req.Customer.ID, created.ID)
			require.NoError(t, err)
			require.Nil(t, get.Capture)
			replay, err = client.CreateCheckoutSession(ctx, req)
			require.NoError(t, err)
			require.Nil(t, replay.Capture)
			require.Equal(t, completed.PaymentMethodID, replay.PaymentMethodID)
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT rail_state::text FROM openrails.checkout_sessions WHERE merchant_id=$1 AND id=$2`, mid.UUID(), created.ID.UUID()).Scan(&state))
			require.NotContains(t, state, "secret_ciphertext")
			require.NotContains(t, state, confirm.Payment.Capture.Token)
			var methods, financial int
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM openrails.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, mid.UUID(), req.Customer.ID.UUID()).Scan(&methods))
			require.Equal(t, 1, methods)
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM openrails.payments WHERE merchant_id=$1 AND customer_id=$2)+(SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1 AND customer_id=$2)+(SELECT count(*) FROM openrails.ledger_accounts WHERE merchant_id=$1 AND customer_id=$2)`, mid.UUID(), req.Customer.ID.UUID()).Scan(&financial))
			require.Zero(t, financial)
		})
	}
	t.Run("lost caller response then cold runtime", func(t *testing.T) {
		req := request()
		before := g.count()
		lost := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithHTTPClient(&http.Client{Transport: loseCaptureReply{http.DefaultTransport, t}}))
		_, err := lost.CreateCheckoutSession(ctx, req)
		require.Error(t, err)
		require.Equal(t, before+1, g.count())
		_, fresh := newEmbedded()
		recovered, err := fresh.CreateCheckoutSession(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, recovered.Capture)
		require.Equal(t, before+1, g.count())
		g.mu.Lock()
		expected := g.sessions[recovered.Capture.SessionID].Secret
		g.mu.Unlock()
		require.Equal(t, expected, recovered.Capture.SDKAuthorization)
	})
	t.Run("concurrent preparations expose only CAS winner", func(t *testing.T) {
		req := request()
		before := g.count()
		g.mu.Lock()
		g.barrier = make(chan struct{})
		g.arrived = 0
		g.mu.Unlock()
		type result struct {
			session *openrails.CheckoutSession
			err     error
		}
		results := make(chan result, 2)
		for _, client := range []*openrails.Client{remote, embedded} {
			go func() { s, e := client.CreateCheckoutSession(ctx, req); results <- result{s, e} }()
		}
		a, b := <-results, <-results
		require.NoError(t, a.err)
		require.NoError(t, b.err)
		require.Equal(t, before+2, g.count())
		require.Equal(t, a.session.ID, b.session.ID)
		require.Equal(t, a.session.Capture, b.session.Capture)
		_, fresh := newEmbedded()
		got, err := fresh.GetCheckoutSession(ctx, req.Customer.ID, a.session.ID)
		require.NoError(t, err)
		require.Equal(t, a.session.Capture, got.Capture)
	})

	t.Run("unpatched deployment cannot issue browser authority", func(t *testing.T) {
		for _, mode := range []string{"stock", "missing", "disabled"} {
			g.mu.Lock()
			g.preflight = mode
			users := len(g.users)
			g.mu.Unlock()
			before := g.count()
			got, err := remote.CreateCheckoutSession(ctx, request())
			require.Error(t, err)
			require.Nil(t, got)
			require.Equal(t, before, g.count())
			g.mu.Lock()
			require.Len(t, g.users, users)
			g.preflight = ""
			g.mu.Unlock()
		}
	})
	t.Run("account change during metadata read refuses attachment", func(t *testing.T) {
		for _, change := range []string{"psp archive", "custodian archive", "retarget", "profile"} {
			t.Run(change, func(t *testing.T) {
				req := request()
				session, err := remote.CreateCheckoutSession(ctx, req)
				require.NoError(t, err)
				token := g.complete(session.Capture.SessionID)
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				t.Cleanup(unblock)
				g.mu.Lock()
				g.afterMethodRead = func() { close(entered); <-release }
				g.mu.Unlock()
				t.Cleanup(func() { g.mu.Lock(); g.afterMethodRead = nil; g.mu.Unlock() })
				finished := make(chan error, 1)
				go func() {
					_, err := remote.ConfirmCheckoutSession(ctx, session.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: req.Customer.ID, Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: custodian, SessionID: session.Capture.SessionID, Token: token}}})
					finished <- err
				}()
				select {
				case <-entered:
				case err := <-finished:
					t.Fatalf("confirm did not reach metadata: %v", err)
				}
				t.Cleanup(func() {
					_, err := h.sharedPool().Exec(context.WithoutCancel(ctx), `UPDATE openrails.psps SET archived=false,custodian_id=$1 WHERE merchant_id=$2 AND id=$3`, custodian, mid.UUID(), psp)
					require.NoError(t, err)
					_, err = h.sharedPool().Exec(context.WithoutCancel(ctx), `UPDATE openrails.custodians SET archived=false,settings=jsonb_set(settings,'{profile_id}','"capture-profile"') WHERE merchant_id=$1 AND id=$2`, mid.UUID(), custodian)
					require.NoError(t, err)
				})
				switch change {
				case "psp archive":
					_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.psps SET archived=true WHERE merchant_id=$1 AND id=$2`, mid.UUID(), psp)
				case "custodian archive":
					_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.custodians SET archived=true WHERE merchant_id=$1 AND id=$2`, mid.UUID(), custodian)
				case "retarget":
					_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.psps SET custodian_id=NULL WHERE merchant_id=$1 AND id=$2`, mid.UUID(), psp)
				case "profile":
					_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.custodians SET settings=jsonb_set(settings,'{profile_id}','"different-profile"') WHERE merchant_id=$1 AND id=$2`, mid.UUID(), custodian)
				}
				require.NoError(t, err)
				unblock()
				require.Error(t, <-finished)
				var methods int
				require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM openrails.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, mid.UUID(), req.Customer.ID.UUID()).Scan(&methods))
				require.Zero(t, methods)
			})
		}
	})
	t.Run("two deployments sharing vendor account keep payer identities separate", func(t *testing.T) {
		req := request()
		first, err := remote.CreateCheckoutSession(ctx, req)
		require.NoError(t, err)
		schema := "capture_owner_" + uuid.NewString()[:8]
		require.NoError(t, migrate.RunPostgres(ctx, &config.Config{DB: &config.DBConfig{URL: h.SuperDSN, Schema: schema}}))
		rt, _ := newEmbedded(schema)
		runtime := app.HostGraph(rt).Runtime
		other := merchant.ID(uuid.New())
		otherCustodian, otherPSP := uuid.New(), uuid.New()
		_, err = runtime.DB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, other.UUID(), "capture-other")
		require.NoError(t, err)
		require.NoError(t, runtime.DB.RunInMerchantConn(merchant.WithID(ctx, other), func(scoped context.Context) error {
			_, err := runtime.DB.Qx(scoped).Exec(scoped, `INSERT INTO openrails.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,'capture','hyperswitch','test',$3,'{"public_api_key":"capture-public","profile_id":"capture-profile"}','{"api_key":1}')`, otherCustodian, other.UUID(), g.account)
			if err != nil {
				return err
			}
			_, err = runtime.DB.Qx(scoped).Exec(scoped, `INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key,custodian_id) VALUES($1,$2,'nmi','test',$3,'capture',$4)`, otherPSP, other.UUID(), "other-psp", otherCustodian)
			return err
		}))
		_, err = runtime.Merchants.Secrets().Put(ctx, other, name, "capture-fixture-key")
		require.NoError(t, err)
		client, err := rt.Client(openrails.WithMerchantID(other))
		require.NoError(t, err)
		otherReq := req
		otherReq.Payment.PSPID = otherPSP
		second, err := client.CreateCheckoutSession(ctx, otherReq)
		require.NoError(t, err)
		require.NotEqual(t, first.Capture.CustomerID, second.Capture.CustomerID)
		require.NotEqual(t, first.ID, second.ID)
		token := g.complete(first.Capture.SessionID)
		_, err = client.ConfirmCheckoutSession(ctx, second.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: req.Customer.ID, Payment: openrails.ConfirmPayment{Capture: &openrails.CustodianCaptureReference{CustodianID: otherCustodian, SessionID: second.Capture.SessionID, Token: token}}})
		require.Error(t, err)
	})
	t.Run("copied or corrupt ciphertext never reissues a session", func(t *testing.T) {
		one, two := request(), request()
		a, err := remote.CreateCheckoutSession(ctx, one)
		require.NoError(t, err)
		b, err := remote.CreateCheckoutSession(ctx, two)
		require.NoError(t, err)
		before := g.count()
		_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.checkout_sessions b SET rail_state=jsonb_set(b.rail_state,'{capture,secret_ciphertext}',a.rail_state#>'{capture,secret_ciphertext}') FROM openrails.checkout_sessions a WHERE a.merchant_id=$1 AND b.merchant_id=$1 AND a.id=$2 AND b.id=$3`, mid.UUID(), a.ID.UUID(), b.ID.UUID())
		require.NoError(t, err)
		_, err = remote.GetCheckoutSession(ctx, two.Customer.ID, b.ID)
		require.Error(t, err)
		_, err = remote.CreateCheckoutSession(ctx, two)
		require.Error(t, err)
		require.Equal(t, before, g.count())
		_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.checkout_sessions SET rail_state=jsonb_set(rail_state,'{capture,secret_ciphertext}','"corrupt"') WHERE merchant_id=$1 AND id=$2`, mid.UUID(), b.ID.UUID())
		require.NoError(t, err)
		_, err = remote.GetCheckoutSession(ctx, two.Customer.ID, b.ID)
		require.Error(t, err)
		require.Equal(t, before, g.count())
	})
	t.Run("expired session clears ciphertext and never reissues", func(t *testing.T) {
		req := request()
		rt, client := newEmbedded()
		session, err := client.CreateCheckoutSession(ctx, req)
		require.NoError(t, err)
		before := g.count()
		clock := clockwork.NewFakeClockAt(session.Capture.ExpiresAt.Add(time.Second))
		app.HostGraph(rt).Runtime.CheckoutSessionService.SetClock(clock)
		expired, err := client.GetCheckoutSession(ctx, req.Customer.ID, session.ID)
		require.NoError(t, err)
		require.Equal(t, "expired", expired.Status)
		require.Nil(t, expired.Capture)
		replay, err := client.CreateCheckoutSession(ctx, req)
		require.NoError(t, err)
		require.Equal(t, "expired", replay.Status)
		require.Nil(t, replay.Capture)
		require.Equal(t, before, g.count())
		var secret bool
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (rail_state->'capture')?'secret_ciphertext' FROM openrails.checkout_sessions WHERE merchant_id=$1 AND id=$2`, mid.UUID(), session.ID.UUID()).Scan(&secret))
		require.False(t, secret)
	})
	t.Run("active secret refuses archive; terminal bindings restore", func(t *testing.T) {
		rt := surface.App().Runtime
		var artifact bytes.Buffer
		err := merchantarchive.Export(ctx, rt.DB, mid, &artifact)
		require.Error(t, err)
		var archiveErr *merchantarchive.Error
		require.ErrorAs(t, err, &archiveErr)
		require.Equal(t, "unsupported_state", archiveErr.Code)
		require.Equal(t, "checkout_sessions", archiveErr.Table)
		require.Zero(t, artifact.Len(), "active secret state refuses before archive header")
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(scoped context.Context) error {
			_, err := rt.DB.Gen(scoped).ExpireCheckoutSessions(scoped, gen.ExpireCheckoutSessionsParams{MerchantID: mid.UUID(), Now: time.Now().Add(time.Hour), RowLimit: 100})
			return err
		}))
		artifact.Reset()
		require.NoError(t, merchantarchive.Export(ctx, rt.DB, mid, &artifact))
		require.NotContains(t, artifact.String(), "secret_ciphertext")
		require.NotContains(t, artifact.String(), "capture-fixture-key")
		g.mu.Lock()
		for _, session := range g.sessions {
			require.NotContains(t, artifact.String(), session.Secret)
			require.NotContains(t, artifact.String(), session.Token)
		}
		g.mu.Unlock()
		schema := "capture_archive_" + uuid.NewString()[:8]
		require.NoError(t, migrate.RunPostgres(ctx, &config.Config{DB: &config.DBConfig{URL: h.SuperDSN, Schema: schema}}))
		restored, client := newEmbedded(schema)
		target := app.HostGraph(restored).Runtime.DB
		_, err = target.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), "restored-capture")
		require.NoError(t, err)
		_, err = merchantarchive.Restore(ctx, target, mid, bytes.NewReader(artifact.Bytes()))
		require.NoError(t, err)
		var second bytes.Buffer
		require.NoError(t, merchantarchive.Export(ctx, target, mid, &second))
		require.Equal(t, artifact.String(), second.String())
		before := g.count()
		var rows []gen.OpenrailsCheckoutSession
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(scoped context.Context) error {
			// Fixture reads retain all completed setup ids; public Client performs the replay.
			raw, err := rt.DB.Qx(scoped).Query(scoped, `SELECT id,customer_id FROM openrails.checkout_sessions WHERE merchant_id=$1 AND status='succeeded'`, mid.UUID())
			if err != nil {
				return err
			}
			defer raw.Close()
			for raw.Next() {
				var row gen.OpenrailsCheckoutSession
				if err := raw.Scan(&row.ID, &row.CustomerID); err != nil {
					return err
				}
				rows = append(rows, row)
			}
			return raw.Err()
		}))
		require.Len(t, rows, 2)
		for _, row := range rows {
			got, err := client.GetCheckoutSession(ctx, openrails.CustomerID(row.CustomerID), openrails.CheckoutSessionID(row.ID))
			require.NoError(t, err)
			require.Equal(t, "succeeded", got.Status)
			require.NotNil(t, got.PaymentMethodID)
			require.Nil(t, got.Capture)
		}
		require.Equal(t, before, g.count(), "restore and terminal replay never call vendor mutation")
	})

}
