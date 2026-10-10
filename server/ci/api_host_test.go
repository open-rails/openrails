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

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
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
	dns := newTXTServer(t)
	cp := f.newVaultServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.PublicBillingBaseURL = "https://" + shared
		cfg.Engine.DashboardBaseURL = "https://" + console
		deps.Engine.DNSResolver = dns.resolver()
	})
	handler, err := standaloneHandler(cp)
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
			r.Header.Set("OpenRails-Merchant", selector)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	type shop struct {
		id            billing.MerchantID
		slug, session string
	}
	provision := func(prefix string) shop {
		owner := newAccount(t, cp)
		slug := uniqueName(prefix)
		m, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
		require.NoError(t, err)
		return shop{m.MerchantID, slug, authtest.SignIn(t, cp.AuthKit(), owner).AccessToken}
	}
	victim, squatter := provision("victim"), provision("squatter")
	key, err := cp.CreateMerchantAPIKey(ctx, server.OperatorActor(), victim.id, billing.CreateAPIKeyParams{Name: "backend", Role: "owner"})
	require.NoError(t, err)
	victimKey := key.Secret
	works := func(host string, s shop) int {
		t.Helper()
		return on(host, s.session, http.MethodGet, "/v1/admin/findings", s.slug, nil).Code
	}
	claim := func(s shop, host string) (*billing.MerchantAPIHost, error) {
		return cp.ClaimMerchantAPIHost(ctx, s.id, host)
	}
	verify := func(s shop) (*billing.MerchantAPIHost, error) {
		return cp.VerifyMerchantAPIHost(ctx, s.id)
	}
	for _, route := range []struct{ method, path string }{{http.MethodPut, "/v1/admin/api-host"}, {http.MethodPost, "/v1/admin/api-host/verify"}} {
		w := on(shared, victim.session, route.method, route.path, victim.slug, map[string]string{"api_host": "shop.victim.e2e.test"})
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, w.Code, "%s %s: a claim is the hosted product's: %s", route.method, route.path, w.Body.String())
	}

	const domain = "shop.victim.e2e.test"
	record := "_openrails-challenge." + domain

	// A squatter's claim routes nothing and cannot be proven.
	state, err := claim(squatter, domain)
	require.NoError(t, err)
	squatterClaim := state.Claim
	require.NotNil(t, squatterClaim)
	require.Equal(t, "TXT", squatterClaim.DNSRecord.Type)
	require.Equal(t, record, squatterClaim.DNSRecord.Name)
	unproven, err := verify(squatter)
	require.ErrorIs(t, err, server.ErrAPIHostUnproven)
	require.Equal(t, squatterClaim.DNSRecord, unproven.Claim.DNSRecord, "the refusal names the record to publish")
	require.Equal(t, http.StatusOK, works(domain, victim), "an unproven host pins no merchant")

	// The domain's owner claims it, publishes its token and proves it.
	state, err = claim(victim, domain)
	require.NoError(t, err)
	victimToken := state.Claim.DNSRecord.Value
	require.NotEqual(t, squatterClaim.DNSRecord.Value, victimToken)
	w := on(shared, victim.session, http.MethodGet, "/v1/admin/api-host", victim.slug, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var pending billing.MerchantAPIHost
	require.NoError(t, json.NewDecoder(w.Body).Decode(&pending))
	require.Nil(t, pending.APIHost, "a claim is not the api_host")
	require.Equal(t, domain, pending.Claim.APIHost)
	require.Equal(t, http.StatusOK, works(domain, squatter), "a claim pins nothing")
	dns.publish(record, victimToken)
	_, err = verify(squatter)
	require.ErrorIs(t, err, server.ErrAPIHostUnproven)
	bound, err := verify(victim)
	require.NoError(t, err)
	require.Equal(t, domain, *bound.APIHost)
	require.Nil(t, bound.Claim)
	require.Equal(t, http.StatusOK, works(domain, victim))
	require.Equal(t, http.StatusOK, on(domain, victimKey, http.MethodGet, "/v1/admin/findings", "", nil).Code)
	require.Equal(t, http.StatusForbidden, works(domain, squatter), "the proven host routes to its merchant only")

	// Even with its token in the record, a squatter cannot take a held host.
	dns.publish(record, squatterClaim.DNSRecord.Value)
	_, err = verify(squatter)
	require.ErrorIs(t, err, server.ErrAPIHostTaken)
	_, err = claim(squatter, domain)
	require.ErrorIs(t, err, server.ErrAPIHostTaken)

	// The deployment's own hosts and bare addresses are never claimable.
	for _, host := range []string{shared, "API.E2E.Test:443", console, "127.0.0.1"} {
		_, err = claim(squatter, host)
		require.ErrorIs(t, err, server.ErrAPIHostReserved, host)
	}
	_, err = claim(squatter, "203.0.113.7")
	require.ErrorIs(t, err, server.ErrInvalidAPIHost)
	refused := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), code)
	}

	// The configuration document binds no host: only a proven claim does.
	w = on(shared, squatter.session, http.MethodGet, "/v1/admin/configuration", squatter.slug, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	configuration := map[string]any{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&configuration))
	refused(on(shared, squatter.session, http.MethodPatch, "/v1/admin/configuration", squatter.slug,
		map[string]any{"expected_revision": configuration["revision"], "api_host": domain}), http.StatusBadRequest, "unknown_field")

	// Control: the squatter proves a domain it does control.
	const own = "shop.squatter.e2e.test"
	state, err = claim(squatter, own)
	require.NoError(t, err)
	dns.publish("_openrails-challenge."+own, state.Claim.DNSRecord.Value)
	_, err = verify(squatter)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, works(own, squatter))

	// Giving a host up needs no proof, and it stops routing at once.
	state, err = claim(victim, "")
	require.NoError(t, err)
	require.Nil(t, state.APIHost)
	require.Equal(t, http.StatusOK, works(domain, squatter))
}
