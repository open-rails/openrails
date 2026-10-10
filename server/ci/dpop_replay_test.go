//go:build e2e && integration

package ci_test

import (
	"net/http"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/server"
)

// A DPoP proof is spent once across every instance, recorded in PostgreSQL:
// with no Redis at all, and while a declared Redis is down.
func TestDPoPProofSpentOnceAcrossInstances(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://dpop-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	const findings = "/v1/admin/findings"
	for name, redisAddr := range map[string]string{"no Redis": "", "Redis down": "127.0.0.1:1"} {
		t.Run(name, func(t *testing.T) {
			shop := uniqueName("dpop")
			edit := func(cfg *server.Config, _ *server.Deps) {
				cfg.ResourceServer = &server.ResourceServerConfig{
					Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
					TrustedIssuers: []server.TrustedIssuerConfig{{Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop}, Permissions: []string{staffperm.Read}}},
				}
				if redisAddr != "" {
					cfg.Engine.Redis = &openrails.RedisConfig{Addr: redisAddr}
				}
			}
			a, b := f.newServer(t, edit), f.newServer(t, edit)
			provision(t, a, shop)
			ha, err := standaloneHandler(a)
			require.NoError(t, err)
			hb, err := standaloneHandler(b)
			require.NoError(t, err)

			browser := newBrowserKey(t)
			bound := host.mint(t, func(c jwt.MapClaims) { c["cnf"] = map[string]string{"jkt": browser.jkt} })
			call := func(h http.Handler, proof string) (int, string) {
				w := serve(h, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: proof})
				return w.Code, w.Body.String()
			}
			w := serve(ha, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: browser.proof(t, http.MethodGet, findings, bound, "")})
			nonce := w.Header().Get("DPoP-Nonce")
			require.NotEmpty(t, nonce, w.Body.String())

			proof := browser.proof(t, http.MethodGet, findings, bound, nonce)
			code, body := call(ha, proof)
			require.Equal(t, http.StatusOK, code, body)
			code, body = call(hb, proof)
			require.Equal(t, http.StatusUnauthorized, code, "the other instance refuses the spent proof: %s", body)
			code, _ = call(ha, proof)
			require.Equal(t, http.StatusUnauthorized, code, "and so does the first")
			code, body = call(hb, browser.proof(t, http.MethodGet, findings, bound, nonce))
			require.Equal(t, http.StatusOK, code, "a fresh proof is accepted anywhere: %s", body)
		})
	}
}
