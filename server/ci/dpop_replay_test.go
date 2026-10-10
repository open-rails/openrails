//go:build e2e && integration

package ci_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/server"
)

// A DPoP proof is spent once: AuthKit keeps spent proofs in Redis, shared by
// every instance, or without it in each instance's memory. One instance
// without Redis refuses a replay; two sharing a Redis refuse it on either;
// while a declared Redis is down, each instance still refuses its own.
func TestDPoPProofSpentOnce(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://dpop-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	const findings = "/v1/admin/findings"
	shared := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if shared == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	for _, tc := range []struct {
		name, redis string
		instances   int
	}{
		{name: "one instance without Redis", instances: 1},
		{name: "two instances sharing Redis", redis: shared, instances: 2},
		{name: "one instance while Redis is down", redis: "127.0.0.1:1", instances: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shop := uniqueName("dpop")
			edit := func(cfg *server.Config, _ *server.Deps) {
				cfg.ResourceServer = &server.ResourceServerConfig{
					Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
					TrustedIssuers: []server.TrustedIssuerConfig{{Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop}, Permissions: []string{staffperm.BillingRead}}},
				}
				if tc.redis != "" {
					cfg.Engine.Redis = &openrails.RedisConfig{Addr: tc.redis}
				}
			}
			handlers := make([]http.Handler, tc.instances)
			for i := range handlers {
				srv := f.newServer(t, edit)
				if i == 0 {
					provision(t, srv, shop)
				}
				h, err := standaloneHandler(srv)
				require.NoError(t, err)
				handlers[i] = h
			}
			a, b := handlers[0], handlers[len(handlers)-1]

			browser := newBrowserKey(t)
			bound := host.mint(t, func(c jwt.MapClaims) { c["cnf"] = map[string]string{"jkt": browser.jkt} })
			call := func(h http.Handler, proof string) (int, string) {
				w := serve(h, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: proof})
				return w.Code, w.Body.String()
			}
			w := serve(a, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: browser.proof(t, http.MethodGet, findings, bound, "")})
			nonce := w.Header().Get("DPoP-Nonce")
			require.NotEmpty(t, nonce, w.Body.String())

			proof := browser.proof(t, http.MethodGet, findings, bound, nonce)
			code, body := call(a, proof)
			require.Equal(t, http.StatusOK, code, body)
			code, body = call(b, proof)
			require.Equal(t, http.StatusUnauthorized, code, "the spent proof was admitted: %s", body)
			code, _ = call(a, proof)
			require.Equal(t, http.StatusUnauthorized, code)
			code, body = call(b, browser.proof(t, http.MethodGet, findings, bound, nonce))
			require.Equal(t, http.StatusOK, code, "a fresh proof: %s", body)
		})
	}
}
