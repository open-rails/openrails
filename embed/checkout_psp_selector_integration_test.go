//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// #848: the checkout wire contract speaks PSP vocabulary. The pre-gate must
// accept a PSP key ("mobius-sandbox"), keep accepting a rail kind when exactly
// one PSP is armed on it, 400 an ambiguous rail kind NAMING the armed keys,
// and 400 unknown selectors. Boundary technique per #775's pre-gate pin: a
// fabricated price_id distinguishes "passed the gate" (400 "price not found",
// deeper) from a gate rejection.
func TestCheckoutPreGate_PSPKeySelector(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mpsp%d", nano)

	rt, id := bootManifestRuntimeWithRailAccounts(t, ctx, dsn, slug, map[string]embed.PSPConfig{
		// Two armed NMI PSPs: the rail kind is ambiguous, the keys are exact.
		"mobius-sandbox": {
			"nmi": {
				AccountID: fmt.Sprintf("gw-a-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-a-" + slug},
			},
		},
		"paykings": {
			"nmi": {
				AccountID: fmt.Sprintf("gw-b-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-b-" + slug},
			},
		},
		// One armed CCBill PSP: the bare rail kind stays unambiguous.
		"ccbill": {
			"ccbill": {
				AccountID: fmt.Sprintf("94%04d-0000", nano%10_000),
				Secrets: map[string]string{
					"datalink_username": "dl-user-" + slug,
					"datalink_password": "dl-pass-" + slug,
				},
			},
		},
	})

	authn := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return &billingauth.DelegatedPrincipal{MerchantID: id.UUID().String(), SubjectID: uuid.NewString()}, nil
	})
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Customer: true}, DelegatedAuthenticator: authn})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// A fixed price id: a random UUID can spell a Luhn-valid digit run across
	// its hyphenated groups and trip the PAN firewall (seen once in 41 runs);
	// every group here carries a hex letter, so no 13-digit run can form.
	fabricatedPrice := uuid.MustParse("a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")
	post := func(rail string) (int, string) {
		body := fmt.Sprintf(`{"price_id":%q,"payment":{"rail":%q}}`, openrails.PriceID(fabricatedPrice).String(), rail)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/me/checkout", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer host-credential")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(raw)
	}

	// The pre-gate's refusal for a rail with nothing armed. Named once: the
	// negative assertions below only guard the gate while they spell it the
	// same way the positive one does.
	const unarmedRailRefusal = "has no armed PSP"

	// PSP key: passes the gate, fails deeper on the fabricated price.
	status, raw := post("mobius-sandbox")
	require.Equal(t, http.StatusBadRequest, status, raw)
	require.NotContains(t, raw, unarmedRailRefusal, "PSP key must pass the pre-gate")
	require.NotContains(t, raw, "ambiguous rail")
	require.Contains(t, raw, "price not found", raw)

	// Rail kind with exactly one armed PSP: still accepted.
	status, raw = post("ccbill")
	require.Equal(t, http.StatusBadRequest, status, raw)
	require.NotContains(t, raw, unarmedRailRefusal, "unambiguous rail kind must pass the pre-gate")
	require.Contains(t, raw, "price not found", raw)

	// Rail kind with two armed PSPs: rejected, NAMING the keys.
	status, raw = post("nmi")
	require.Equal(t, http.StatusBadRequest, status, raw)
	require.Contains(t, raw, "ambiguous rail", raw)
	require.Contains(t, raw, "mobius-sandbox", raw)
	require.Contains(t, raw, "paykings", raw)

	// Unknown selector: rejected.
	status, raw = post("bogus")
	require.Equal(t, http.StatusBadRequest, status, raw)
	require.Contains(t, raw, "unknown payment provider", raw)

	// Rail kind with nothing armed: rejected (fail closed).
	status, raw = post("solana")
	require.Equal(t, http.StatusBadRequest, status, raw)
	require.Contains(t, raw, unarmedRailRefusal, raw)
}
