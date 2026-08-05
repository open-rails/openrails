//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#288 processor routing, driven end to end over the REAL checkout surface:
// the merchant's declared policy decides which PSP a session lands on before
// the session exists, unavailable candidates are fallen through with a stated
// reason, the decision is persisted on the row, and the dry-run endpoint
// returns the SAME decision without creating anything.

type routingFixture struct {
	merchantID    merchant.ID
	customerURL   string
	merchantURL   string
	priceID       string
	buyerUsername string
}

// bootRoutingFixture arms the given PSPs + routing policy on a fresh mode-1
// merchant, seeds a price linked to every PSP named, and mounts both the buyer
// checkout surface and the merchant surface.
func bootRoutingFixture(
	t *testing.T,
	ctx context.Context,
	dsn, slug string,
	psps map[string]embed.PSPConfig,
	routing []embed.CheckoutRoutingRuleConfig,
) routingFixture {
	t.Helper()

	cfg := sandboxModeConfig(dsn, config.MerchantSourceManifest)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{
		DisplayName:     slug,
		PSPs:            psps,
		CheckoutRouting: routing,
	})
	require.NoError(t, err)
	cleanupCCBillWebhookMerchant(t, id)
	t.Cleanup(func() {
		appDB := dbtest.OpenMerchantDB(t, id.UUID())
		_, _ = appDB.Pool().Exec(context.Background(),
			`DELETE FROM openrails.merchant_configurations WHERE merchant_id = $1`, id.UUID())
	})

	// The catalog is pushed with only the ccbill link (operator-owned, so the
	// push validates nothing remotely), then the full psp_links map is written
	// directly. Declaring an nmi plan_id in the manifest would make the push
	// find-or-create a recurring plan against a live gateway — routing is what
	// is under test here, not catalog push.
	flexID, formName := seedCCBillWebhookCatalog(t, ctx, cfg, slug)
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	links, err := json.Marshal(map[string]map[string]string{
		"ccbill":   {"rail": "ccbill", "flex_id": flexID, "form_name": formName},
		"mobius":   {"rail": "nmi", "plan_id": "plan-" + slug},
		"paykings": {"rail": "nmi", "plan_id": "plan-old-" + slug},
	})
	require.NoError(t, err)
	tag, err := appDB.Pool().Exec(ctx,
		`UPDATE openrails.prices SET psp_links = $2::jsonb WHERE merchant_id = $1`, id.UUID(), string(links))
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "exactly one seeded price must be linked")

	username := "routing_" + uuid.NewString()[:8]
	userID := seedProfileUser(t, ctx, dsn, username)
	email := username + "@test.example.com"
	userAuthn := billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{UserID: userID, Email: email, EmailVerified: true}, nil
	})
	delegated := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return &billingauth.DelegatedPrincipal{MerchantID: id.UUID().String(), SubjectID: userID, Email: email, EmailVerified: true, Username: username}, nil
	})
	buyerHandler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:              []embed.RouteSet{embed.RouteSetCheckout, embed.RouteSetCustomer},
		Authenticator:          userAuthn,
		DelegatedAuthenticator: delegated,
	})
	require.NoError(t, err)
	buyerServer := httptest.NewServer(buyerHandler)
	t.Cleanup(buyerServer.Close)

	merchantHandler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets: []embed.RouteSet{embed.RouteSetPaymentProviders, embed.RouteSetMerchantAPI},
		Gate:      allowAllGate{id: id},
	})
	require.NoError(t, err)
	merchantServer := httptest.NewServer(merchantHandler)
	t.Cleanup(merchantServer.Close)

	return routingFixture{
		merchantID:    id,
		customerURL:   buyerServer.URL,
		merchantURL:   merchantServer.URL,
		priceID:       fetchCCBillPriceID(t, buyerServer.URL),
		buyerUsername: username,
	}
}

type routingDryRunResponse struct {
	Policy     string `json:"policy"`
	Rule       *int   `json:"rule"`
	Selected   string `json:"selected"`
	Rail       string `json:"rail"`
	Mode       string `json:"mode"`
	Candidates []struct {
		Selector string `json:"selector"`
		Rail     string `json:"rail"`
		Skip     string `json:"skip"`
	} `json:"candidates"`
	RoutingReason json.RawMessage `json:"routing_reason"`
}

func routingDryRun(t *testing.T, fx routingFixture, selector string) routingDryRunResponse {
	t.Helper()
	body := fmt.Sprintf(`{"price_id":%q,"country":"US","selector":%q}`, fx.priceID, selector)
	req, err := http.NewRequest(http.MethodPost, fx.merchantURL+"/v1/merchant/payment-providers/routing/dry-run", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out routingDryRunResponse
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return out
}

// openRoutedCheckoutSession posts a checkout WITHOUT payment.rail — the
// routing request — and returns the created session id.
func openRoutedCheckoutSession(t *testing.T, fx routingFixture) string {
	t.Helper()
	body := fmt.Sprintf(`{"price_id":%q,"mode":"subscription","payment":{"email":%q,"first_name":"Routing","last_name":"Buyer","address1":"123 Test St","city":"Denver","state":"CO","zip":"80202","country":"US"}}`,
		fx.priceID, fx.buyerUsername+"@test.example.com")
	req, err := http.NewRequest(http.MethodPost, fx.customerURL+"/v1/me/checkout", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var session struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Rail   string `json:"rail"`
	}
	require.NoError(t, json.Unmarshal(raw, &session), string(raw))
	require.Equal(t, "requires_action", session.Status, string(raw))
	return session.ID
}

func sessionRoutingReason(t *testing.T, ctx context.Context, mid merchant.ID, sessionID string) (rail string, reason json.RawMessage) {
	t.Helper()
	appDB := dbtest.OpenMerchantDB(t, mid.UUID())
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT rail, routing_reason FROM openrails.checkout_sessions WHERE merchant_id = $1`,
		mid.UUID()).Scan(&rail, &reason))
	return rail, reason
}

// A declared policy reorders the candidates, falls through an ARCHIVED PSP with
// a stated reason, records the decision on the session row, and the dry-run
// endpoint reproduces that decision exactly without creating anything.
func TestCheckoutRoutingPolicyDecidesRecordsAndDryRuns(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("rtpol%d", nano)
	ccbillAccount := fmt.Sprintf("96%04d-0000", nano%10_000)

	fx := bootRoutingFixture(t, ctx, dsn, slug,
		map[string]embed.PSPConfig{
			"ccbill": {"ccbill": {
				AccountID: ccbillAccount,
				Secrets:   map[string]string{"salt": "test-salt-" + slug},
			}},
			"mobius": {"nmi": {
				AccountID: fmt.Sprintf("gw-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-" + slug},
			}},
			// Retired: declared, archived, and therefore never routable.
			"paykings": {"nmi": {
				AccountID: fmt.Sprintf("gw-old-%d", nano),
				Archived:  true,
				Secrets:   map[string]string{"security_key": "sk-old-" + slug},
			}},
		},
		[]embed.CheckoutRoutingRuleConfig{
			// Deliberately NOT the default order: the retired PSP first, then
			// ccbill, then nmi. If the policy were ignored the default order
			// would put nmi (mobius) ahead of ccbill.
			{Prefer: []string{"paykings", "ccbill", "mobius"}},
		},
	)

	// The dry run explains the decision without creating anything.
	trace := routingDryRun(t, fx, "")
	require.Equal(t, "merchant", trace.Policy)
	require.NotNil(t, trace.Rule)
	require.Equal(t, 0, *trace.Rule)
	require.Equal(t, "ccbill", trace.Selected)
	require.Equal(t, "ccbill", trace.Rail)
	require.Equal(t, "subscription", trace.Mode)
	require.Len(t, trace.Candidates, 3)
	require.Equal(t, "paykings", trace.Candidates[0].Selector)
	require.Equal(t, "not_armed", trace.Candidates[0].Skip, "an archived PSP is retired, not unknown")
	require.Equal(t, "ccbill", trace.Candidates[1].Selector)
	require.Empty(t, trace.Candidates[1].Skip)
	require.Equal(t, "mobius", trace.Candidates[2].Selector)
	require.Empty(t, trace.Candidates[2].Skip)

	var appDB = dbtest.OpenMerchantDB(t, fx.merchantID.UUID())
	var sessionCount int
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.checkout_sessions WHERE merchant_id = $1`, fx.merchantID.UUID()).Scan(&sessionCount))
	require.Zero(t, sessionCount, "a dry run must not create a session")

	// The real checkout, with NO payment.rail, makes the same decision.
	sessionID := openRoutedCheckoutSession(t, fx)
	require.NotEmpty(t, sessionID)

	rail, reason := sessionRoutingReason(t, ctx, fx.merchantID, sessionID)
	require.Equal(t, "ccbill", rail)
	require.NotEmpty(t, reason, "the routed session must record WHY it chose this PSP")
	require.JSONEq(t, string(trace.RoutingReason), string(reason),
		"the dry-run trace must be the decision a real session records")
}

// With no policy declared, routing reproduces the historical hardcoded order:
// stripe, then nmi, then ccbill, then solana.
func TestCheckoutRoutingDefaultOrderMatchesLegacyPreference(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("rtdef%d", nano)
	ccbillAccount := fmt.Sprintf("97%04d-0000", nano%10_000)

	fx := bootRoutingFixture(t, ctx, dsn, slug,
		map[string]embed.PSPConfig{
			"ccbill": {"ccbill": {
				AccountID: ccbillAccount,
				Secrets:   map[string]string{"salt": "test-salt-" + slug},
			}},
			"mobius": {"nmi": {
				AccountID: fmt.Sprintf("gw-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-" + slug},
			}},
		},
		nil, // no policy at all
	)

	trace := routingDryRun(t, fx, "")
	require.Equal(t, "default", trace.Policy)
	require.Nil(t, trace.Rule)
	// nmi outranks ccbill in the built-in order, and the bare rail kind
	// resolves to the one armed PSP's key.
	require.Equal(t, "mobius", trace.Selected)
	require.Equal(t, "nmi", trace.Rail)

	order := make([]string, 0, len(trace.Candidates))
	for _, c := range trace.Candidates {
		order = append(order, c.Rail)
	}
	require.Equal(t, []string{"stripe", "nmi", "ccbill", "solana"}, order,
		"the default candidate order is the historical hardcoded one")
	require.Equal(t, "not_armed", trace.Candidates[0].Skip)
	require.Empty(t, trace.Candidates[1].Skip)
	require.Empty(t, trace.Candidates[2].Skip)
	require.Equal(t, "not_armed", trace.Candidates[3].Skip)

	// MODE-2 PARITY: the same policy declared over the config API takes effect
	// on the next decision, and reorders the very order pinned above.
	putMerchantRoutingPolicy(t, fx, `{"checkout_routing":[{"match":{"currency":"usd"},"prefer":["ccbill","mobius"]}]}`)
	trace = routingDryRun(t, fx, "")
	require.Equal(t, "merchant", trace.Policy)
	require.NotNil(t, trace.Rule)
	require.Equal(t, 0, *trace.Rule)
	require.Equal(t, "ccbill", trace.Selected)
	require.Equal(t, []string{"ccbill", "mobius"}, []string{trace.Candidates[0].Selector, trace.Candidates[1].Selector})

	// A rule whose conditions do not hold is not reached: EUR-only, USD price.
	putMerchantRoutingPolicy(t, fx, `{"checkout_routing":[{"match":{"currency":"eur"},"prefer":["ccbill"]}]}`)
	trace = routingDryRun(t, fx, "")
	require.Equal(t, "default", trace.Policy, "no rule matched, so the built-in order decides")
	require.Equal(t, "mobius", trace.Selected)

	// A malformed policy is refused, not half-applied.
	req, err := http.NewRequest(http.MethodPut, fx.merchantURL+"/v1/merchant/settings",
		strings.NewReader(`{"checkout_routing":[{"prefer":["mobius"]},{"match":{"currency":"eur"},"prefer":["ccbill"]}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "unreachable", string(raw))
}

// putMerchantRoutingPolicy installs a routing policy over the mode-2 config API.
func putMerchantRoutingPolicy(t *testing.T, fx routingFixture, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, fx.merchantURL+"/v1/merchant/settings", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

// #848 is untouched: a bare rail kind with two armed PSPs is still refused when
// the CALLER names it, and routing skips it rather than picking one at random.
func TestCheckoutRoutingKeepsAmbiguousSelectorRefusal(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("rtamb%d", nano)
	ccbillAccount := fmt.Sprintf("98%04d-0000", nano%10_000)

	fx := bootRoutingFixture(t, ctx, dsn, slug,
		map[string]embed.PSPConfig{
			"ccbill": {"ccbill": {
				AccountID: ccbillAccount,
				Secrets:   map[string]string{"salt": "test-salt-" + slug},
			}},
			"mobius": {"nmi": {
				AccountID: fmt.Sprintf("gw-a-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-a-" + slug},
			}},
			"paykings": {"nmi": {
				AccountID: fmt.Sprintf("gw-b-%d", nano),
				Secrets:   map[string]string{"security_key": "sk-b-" + slug},
			}},
		},
		nil,
	)

	// Routing: the ambiguous rail kind is SKIPPED with its class, and the
	// decision falls through to the unambiguous ccbill PSP.
	trace := routingDryRun(t, fx, "")
	require.Equal(t, "default", trace.Policy)
	require.Equal(t, "ambiguous_selector", trace.Candidates[1].Skip)
	require.Equal(t, "ccbill", trace.Selected)

	// An explicitly NAMED ambiguous rail kind is still a hard refusal —
	// routing does not soften the #848 contract for a caller who asked.
	body := fmt.Sprintf(`{"price_id":%q,"mode":"subscription","payment":{"rail":"nmi"}}`, fx.priceID)
	req, err := http.NewRequest(http.MethodPost, fx.customerURL+"/v1/me/checkout", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "ambiguous rail", string(raw))
}
