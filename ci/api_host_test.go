//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/embedcontrolplane"
	"github.com/open-rails/openrails/internal/hostconfig"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// txtServer is an authoritative DNS server on loopback answering TXT queries
// from the records the test publishes; every other name is NXDOMAIN.
type txtServer struct {
	conn    net.PacketConn
	mu      sync.Mutex
	records map[string][]string
}

func newTXTServer(t *testing.T) *txtServer {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &txtServer{conn: conn, records: map[string][]string{}}
	t.Cleanup(func() { _ = conn.Close() })
	go s.serve()
	return s
}

func (s *txtServer) publish(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[name+"."] = append(s.records[name+"."], value)
}

// resolver sends every lookup to s through Go's own DNS client.
func (s *txtServer) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", s.conn.LocalAddr().String())
	}}
}

func (s *txtServer) serve() {
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		s.mu.Lock()
		values := append([]string(nil), s.records[strings.ToLower(q.Name.String())]...)
		s.mu.Unlock()
		rcode := dnsmessage.RCodeSuccess
		if len(values) == 0 {
			rcode = dnsmessage.RCodeNameError
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RCode: rcode})
		if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
			continue
		}
		ok := true
		for _, v := range values { // one record per value, as each publish adds one
			if q.Type == dnsmessage.TypeTXT && b.TXTResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, dnsmessage.TXTResource{TXT: []string{v}}) != nil {
				ok = false
			}
		}
		if !ok {
			continue
		}
		msg, err := b.Finish()
		if err != nil {
			continue
		}
		_, _ = s.conn.WriteTo(msg, addr)
	}
}

// SEC: a merchant's api_host routes only after the merchant proves it controls
// the domain, with the claim's token in a TXT record at
// _openrails-challenge.<host>. A squatter cannot take a domain it does not
// control, the deployment's own hosts are never claimable, and a proven host
// stays with its merchant.
func TestSecurityAPIHostNeedsProofOfControl(t *testing.T) {
	const shared, console = "api.e2e.test", "console.e2e.test"
	f := newFixture(t)
	ctx := t.Context()
	require.NoError(t, standalonedb.ApplyAuthKit(ctx, f.pool))
	dns := newTXTServer(t)
	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{
			TestMode:             config.CredentialPostureSandbox,
			ProviderWriteMode:    config.ProviderWriteModeReadOnly,
			MerchantConfigHTTP:   true,
			PublicBillingBaseURL: "https://" + shared,
			DashboardBaseURL:     "https://" + console,
			DB:                   &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:        []string{"https://e2e.test"},
		},
		PGXPool:     f.pool,
		River:       embed.RiverManagedByOpenRails(f.schema),
		DNSResolver: dns.resolver(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	cp, err := embedcontrolplane.Attach(ctx, rt, embedcontrolplane.Options{Auth: &hostconfig.AuthConfig{
		Issuer: "http://127.0.0.1/" + f.schema, AllowMemory: true, AllowMissingSenders: true,
		AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
	}})
	require.NoError(t, err)
	handler, err := cp.Handler()
	require.NoError(t, err)

	on := func(host, token, method, path, selector string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		r := httptest.NewRequest(method, "https://"+host+path, &payload)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if selector != "" {
			r.Header.Set("X-OpenRails-Merchant", selector)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	type shop struct{ slug, session string }
	provision := func(prefix string) shop {
		owner := newAccount(t, cp)
		slug := uniqueName(prefix)
		_, err := cp.ProvisionMerchant(ctx, embedcontrolplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: owner.ID})
		require.NoError(t, err)
		return shop{slug, authtest.SignIn(t, cp.Core(), owner).AccessToken}
	}
	victim, squatter := provision("victim"), provision("squatter")
	w := on(shared, victim.session, http.MethodPost, "/v1/merchant/api-keys", victim.slug, map[string]string{"name": "backend", "role": "owner"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	key := map[string]any{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&key))
	victimKey := key["secret"].(string)
	works := func(host string, s shop) int {
		t.Helper()
		return on(host, s.session, http.MethodGet, "/v1/merchant/team", s.slug, nil).Code
	}
	type hostState struct {
		APIHost *string `json:"api_host"`
		Claim   *struct {
			APIHost   string                             `json:"api_host"`
			DNSRecord struct{ Type, Name, Value string } `json:"dns_record"`
		} `json:"claim"`
	}
	decode := func(w *httptest.ResponseRecorder) hostState {
		t.Helper()
		var state hostState
		require.NoError(t, json.NewDecoder(w.Body).Decode(&state), w.Body.String())
		return state
	}
	claim := func(s shop, host string) *httptest.ResponseRecorder {
		return on(shared, s.session, http.MethodPut, "/v1/merchant/api-host", s.slug, map[string]string{"api_host": host})
	}
	verify := func(s shop) *httptest.ResponseRecorder {
		return on(shared, s.session, http.MethodPost, "/v1/merchant/api-host/verify", s.slug, nil)
	}
	refused := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), code)
	}

	const domain = "shop.victim.e2e.test"
	record := "_openrails-challenge." + domain

	// A squatter's claim routes nothing and cannot be proven.
	w = claim(squatter, domain)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	squatterClaim := decode(w).Claim
	require.NotNil(t, squatterClaim)
	require.Equal(t, "TXT", squatterClaim.DNSRecord.Type)
	require.Equal(t, record, squatterClaim.DNSRecord.Name)
	refused(verify(squatter), http.StatusConflict, "api_host_unproven")
	require.Equal(t, http.StatusOK, works(domain, victim), "an unproven host pins no merchant")

	// The domain's owner claims it, publishes its token and proves it.
	w = claim(victim, domain)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	victimToken := decode(w).Claim.DNSRecord.Value
	require.NotEqual(t, squatterClaim.DNSRecord.Value, victimToken)
	w = on(shared, victim.session, http.MethodGet, "/v1/merchant/api-host", victim.slug, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	pending := decode(w)
	require.Nil(t, pending.APIHost, "a claim is not the api_host")
	require.Equal(t, domain, pending.Claim.APIHost)
	require.Equal(t, http.StatusOK, works(domain, squatter), "a claim pins nothing")
	dns.publish(record, victimToken)
	refused(verify(squatter), http.StatusConflict, "api_host_unproven")
	w = verify(victim)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bound := decode(w)
	require.Equal(t, domain, *bound.APIHost)
	require.Nil(t, bound.Claim)
	require.Equal(t, http.StatusOK, works(domain, victim))
	require.Equal(t, http.StatusOK, on(domain, victimKey, http.MethodGet, "/v1/merchant/findings", "", nil).Code)
	require.Equal(t, http.StatusForbidden, works(domain, squatter), "the proven host routes to its merchant only")

	// Even with its token in the record, a squatter cannot take a held host.
	dns.publish(record, squatterClaim.DNSRecord.Value)
	refused(verify(squatter), http.StatusConflict, "api_host_taken")
	refused(claim(squatter, domain), http.StatusConflict, "api_host_taken")

	// The deployment's own hosts and bare addresses are never claimable.
	for _, host := range []string{shared, "API.E2E.Test:443", console, "127.0.0.1"} {
		refused(claim(squatter, host), http.StatusBadRequest, "api_host_reserved")
	}
	refused(claim(squatter, "203.0.113.7"), http.StatusBadRequest, "invalid_api_host")

	// The configuration document keeps or clears the proven host; it binds no other.
	apply := func(s shop, host string) *httptest.ResponseRecorder {
		w := on(shared, s.session, http.MethodGet, "/v1/merchant/configuration", s.slug, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		state := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&state))
		return on(shared, s.session, http.MethodPost, "/v1/merchant/configuration/applications", s.slug,
			map[string]any{"application_id": uuid.NewString(), "expected_revision": state["revision"], "api_host": host})
	}
	refused(apply(squatter, "shop.squatter.e2e.test"), http.StatusConflict, "api_host_requires_proof")
	refused(apply(squatter, domain), http.StatusConflict, "api_host_requires_proof")
	require.Equal(t, http.StatusOK, apply(victim, domain).Code, "restating the proven host")

	// Control: the squatter proves a domain it does control.
	const own = "shop.squatter.e2e.test"
	w = claim(squatter, own)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	dns.publish("_openrails-challenge."+own, decode(w).Claim.DNSRecord.Value)
	w = verify(squatter)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, http.StatusOK, works(own, squatter))

	// Giving a host up needs no proof, and it stops routing at once.
	w = claim(victim, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Nil(t, decode(w).APIHost)
	require.Equal(t, http.StatusOK, works(domain, squatter))
}
