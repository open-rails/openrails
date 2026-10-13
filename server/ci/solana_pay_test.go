//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// A wallet opening a Solana Pay link sends nothing but the order's URL: no
// credential, no header, and whatever Host reached the server. Each order
// answers as its own merchant, and an order nobody has answers 404.
func TestSolanaPayFindsItsMerchantByOrder(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	chain := solanafake.New()
	t.Cleanup(chain.Close)
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: chain.URL()}
		cfg.Engine.PublicBillingBaseURL = "https://" + serverHost
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
	})
	type shop struct {
		slug, name, logo string
		id               billing.MerchantID
	}
	shops := []*shop{
		{slug: uniqueName("sol-a"), name: "Aurora Courses", logo: "https://aurora.e2e.test/logo.png"},
		{slug: uniqueName("sol-b"), name: "Borealis Courses", logo: "https://borealis.e2e.test/logo.png"},
	}
	manifest := "version: 1\nmerchants:\n"
	for _, s := range shops {
		manifest += fmt.Sprintf(`  %s:
    display_name: %s
    settings:
      profile: {logo_url: "%s"}
    psps:
      solana:
        rail: solana
        environment: sandbox
        signer: {mode: local_keypair}
        secrets: {private_key: %s}
        settings: {rpc_provider: public, tokens: {DUSD: {}}}
`, s.slug, s.name, s.logo, solanago.NewWallet().PrivateKey.String())
	}
	path := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	graph, plane := operator.Of(srv)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, plane, path, nil, ""))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)

	// awaiting is an order of the shop's that awaits a Solana Pay transfer:
	// its customer bought the shop's one course and chose to pay from a wallet.
	awaiting := func(s *shop) billing.OrderID {
		t.Helper()
		m, err := graph.Runtime.Merchants.GetBySlug(ctx, s.slug)
		require.NoError(t, err)
		s.id = m.ID
		key := "course-" + uuid.NewString()[:8]
		product, err := handle(t, srv, s.id).CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: s.name + " 101"})
		require.NoError(t, err)
		price, err := handle(t, srv, s.id).CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: 5_000_000, PSPs: map[string]map[string]string{"solana": {}}})
		require.NoError(t, err)
		login := newIssuerKey(t, "https://"+s.slug+".e2e.test")
		trust(t, srv, s.id, login.app(t, "viewer", nil))
		customer := customerOf(t, login, uuid.NewString())
		me := func(method, path string, body any) *httptest.ResponseRecorder {
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			q := rsRequest{method: method, path: "/v1/me" + path, authorization: customer}
			if body != nil {
				q.body, q.idempotencyKey = string(raw), uuid.NewString()
			}
			return serve(handler, q)
		}
		setup := me(http.MethodGet, "/config", nil)
		require.Equal(t, http.StatusOK, setup.Code, setup.Body.String())
		var cfg billing.CustomerConfig
		require.NoError(t, json.Unmarshal(setup.Body.Bytes(), &cfg))
		require.Len(t, cfg.Payment.PSPs, 1, setup.Body.String())
		require.NotNil(t, cfg.Payment.Solana, setup.Body.String())

		created := me(http.MethodPost, "/orders", map[string]any{"lines": []any{map[string]any{"price_id": price.ID}}})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		var order billing.Order
		require.NoError(t, json.Unmarshal(created.Body.Bytes(), &order))
		paid := me(http.MethodPost, "/orders/"+order.ID.String()+"/pay", map[string]any{
			"payment": map[string]any{"token_symbol": "DUSD", "psp_id": cfg.Payment.PSPs[0].PSPID}, "expected_total": "5000000"})
		require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
		require.NoError(t, json.Unmarshal(paid.Body.Bytes(), &order))
		require.NotNil(t, order.Payment.NextAction, paid.Body.String())
		require.Equal(t, "solana:https://"+serverHost+"/v1/orders/"+order.ID.String()+"/solana-pay", *order.Payment.NextAction.URL)
		return order.ID
	}
	// wallet is a wallet's request: no credential, and a Host and a merchant
	// header that name nothing of the order's.
	wallet := func(method string, order string, other billing.MerchantID, body string) *httptest.ResponseRecorder {
		return serve(handler, rsRequest{method: method, path: "https://wallet-" + uuid.NewString()[:8] + ".e2e.test/v1/orders/" + order + "/solana-pay",
			header: map[string]string{retiredMerchantHeader: "id:" + other.String()}, body: body})
	}
	orders := []billing.OrderID{awaiting(shops[0]), awaiting(shops[1])}

	for i, s := range shops {
		other := shops[1-i].id
		got := wallet(http.MethodGet, orders[i].String(), other, "")
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		require.JSONEq(t, fmt.Sprintf(`{"label": %q, "icon": %q}`, s.name+" 101", s.logo), got.Body.String())

		account := solanago.NewWallet().PublicKey().String()
		built := wallet(http.MethodPost, orders[i].String(), other, `{"account":"`+account+`"}`)
		require.Equal(t, http.StatusOK, built.Code, built.Body.String())
		var tx struct {
			Transaction string `json:"transaction"`
		}
		require.NoError(t, json.Unmarshal(built.Body.Bytes(), &tx))
		require.NotEmpty(t, tx.Transaction)
	}

	for _, unknown := range []string{billing.OrderID(uuid.New()).String(), "ord_nothing", strings.TrimPrefix(orders[0].String(), "ord_")} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			got := wallet(method, unknown, shops[0].id, `{"account":"`+solanago.NewWallet().PublicKey().String()+`"}`)
			require.Equal(t, http.StatusNotFound, got.Code, "%s %s: %s", method, unknown, got.Body.String())
			require.Equal(t, billing.CodeResourceNotFound, errorCode(t, got), unknown)
		}
	}
}
