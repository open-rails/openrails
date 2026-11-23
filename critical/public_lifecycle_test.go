//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailsfiber "github.com/open-rails/openrails/adapters/fiber"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestPublicLifecycle(t *testing.T) {
	t.Run("OR-J04-S02/cached-catalog-and-credentials-remain-merchant-bound", func(t *testing.T) {
		a := newAuthorityWorld(t)
		alpha, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "shared-key", DisplayName: "Alpha original"})
		require.NoError(t, err)
		beta, err := a.beta.CreateProduct(t.Context(), billing.CreateProductParams{Key: "shared-key", DisplayName: "Beta private"})
		require.NoError(t, err)
		remote, err := openrails.NewRemote(a.server.URL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{a.alpha.MerchantID(): {APIKey: a.alphaKey.Secret}, a.beta.MerchantID(): {APIKey: a.betaKey.Secret}}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, remote.Close(context.Background())) })
		left, err := remote.Merchant(a.alpha.MerchantID())
		require.NoError(t, err)
		right, err := remote.Merchant(a.beta.MerchantID())
		require.NoError(t, err)
		for _, sample := range []struct {
			client *openrails.Client
			id     billing.ProductID
			name   string
		}{{left, alpha.ID, "Alpha original"}, {right, beta.ID, "Beta private"}, {left, alpha.ID, "Alpha original"}} {
			page, err := sample.client.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"shared-key"}})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			require.Equal(t, sample.id, page.Items[0].ID)
			require.Equal(t, sample.name, page.Items[0].DisplayName)
		}
		_, err = right.GetProduct(t.Context(), alpha.ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		_, err = left.UpdateProduct(t.Context(), alpha.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Alpha changed")})
		require.NoError(t, err)
		fresh, err := left.GetProduct(t.Context(), alpha.ID)
		require.NoError(t, err)
		require.Equal(t, "Alpha changed", fresh.DisplayName, "own mutation invalidates the old cached answer")
		unchanged, err := right.GetProduct(t.Context(), beta.ID)
		require.NoError(t, err)
		require.Equal(t, "Beta private", unchanged.DisplayName)
		wrong := a.remote(t, a.alpha.MerchantID(), a.betaKey.Secret)
		_, err = wrong.GetProduct(t.Context(), beta.ID)
		var mismatch *openrails.BoundMerchantError
		require.ErrorAs(t, err, &mismatch)
		require.NoError(t, remote.Close(t.Context()))
		stillUsable, err := left.GetProduct(t.Context(), alpha.ID)
		require.NoError(t, err, "Remote.Close releases its cache work but does not close the server's engine")
		require.Equal(t, alpha.ID, stillUsable.ID)
	})
	t.Run("OR-J04-S05/close-preserves-borrowed-pool-and-worker-fleet", func(t *testing.T) {
		a := newAuthorityWorld(t)
		w := a.world
		_, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "before-network-close", DisplayName: "Mounted authority control"})
		require.NoError(t, err)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "durable-before-close", DisplayName: "Durable"})
		require.NoError(t, err)
		_, err = w.alpha.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		_, err = w.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{product.Key}})
		require.NoError(t, err)
		require.Error(t, w.engine.Ready(t.Context()))
		fleet := hostFleet(t, w)
		require.NoError(t, fleet.Start(context.Background()))
		ctx, cancel := context.WithCancel(context.Background())
		require.NoError(t, w.engine.Start(ctx, openrails.WithRiverClient(fleet)))
		cancel()
		require.NoError(t, w.engine.Ready(t.Context()), "ending Start's context does not shut down the engine")
		require.NoError(t, w.engine.Close(t.Context()))
		require.NoError(t, w.pool.Ping(t.Context()))
		select {
		case <-fleet.Stopped():
			t.Fatal("closing OpenRails stopped its host-owned fleet")
		default:
		}
		_, readErr := w.alpha.GetProduct(t.Context(), product.ID)
		_, writeErr := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "after-close", DisplayName: "Must not commit"})
		_, batchErr := w.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{product.Key}})
		_, uncachedErr := w.alpha.ListOrders(t.Context(), billing.OrderListParams{})
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		_, localErr := w.local.ReleaseAdmissionsTx(t.Context(), tx, []string{"closed-owner"})
		require.NoError(t, tx.Rollback(t.Context()))
		status, raw := a.request(t, "POST", "/v1/catalog/products", a.alphaKey.Secret, billing.CreateProductParams{Key: "after-network-close", DisplayName: "Must not commit over mounted HTTP"})
		t.Logf("after Close: warm read=%v; write=%v; batched cache=%v; uncached read=%v; local transaction=%v", readErr, writeErr, batchErr, uncachedErr, localErr)
		require.ErrorIs(t, readErr, billing.ErrUnreachable, "closed handles cannot serve cached state")
		require.ErrorIs(t, writeErr, billing.ErrUnreachable, "closed handles cannot commit new mutations")
		require.ErrorIs(t, batchErr, billing.ErrUnreachable)
		require.ErrorIs(t, uncachedErr, billing.ErrUnreachable)
		require.ErrorIs(t, localErr, billing.ErrUnreachable)
		require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
		_, err = a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "after-remote-close", DisplayName: "Closed HTTP owner"})
		require.ErrorIs(t, err, billing.ErrUnreachable)
		reopened, err := openrails.New(t.Context(), w.cfg, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
		client, err := reopened.Merchant(w.alpha.MerchantID())
		require.NoError(t, err)
		kept, err := client.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Durable", kept.DisplayName)
		require.Equal(t, product.ID, kept.ID)
		created, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "reopened-owner", DisplayName: "Reopened"})
		require.NoError(t, err)
		require.NotEqual(t, product.ID, created.ID)
		absent, err := client.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"after-close", "after-network-close", "after-remote-close"}})
		require.NoError(t, err)
		require.Empty(t, absent.Items, "the refused old handle created no durable row")
	})
	t.Run("OR-J04-S06/adapters-preserve-credential-and-encoded-path-boundaries", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		_, price := publicSale(t, a, "adapter-control", 9_990_000, nil)
		order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "adapter-owned-order"})
		require.NoError(t, err)
		selection := openrails.Routes{Prefix: "/billing", Auth: a.auth.Authenticator()}
		missing := selection
		missing.Auth = nil
		bad := selection
		bad.Prefix = "/billing/../escape"
		for _, name := range []string{"http", "gin", "fiber"} {
			t.Run(name, func(t *testing.T) {
				var request func(*http.Request) (*http.Response, error)
				switch name {
				case "http":
					require.Error(t, openrailshttp.Mount(http.NewServeMux(), a.engine, missing))
					require.Error(t, openrailshttp.Mount(http.NewServeMux(), a.engine, bad))
					mux := http.NewServeMux()
					require.NoError(t, openrailshttp.Mount(mux, a.engine, selection))
					server := httptest.NewServer(mux)
					t.Cleanup(server.Close)
					request = func(r *http.Request) (*http.Response, error) {
						r.URL.Scheme = "http"
						r.URL.Host = strings.TrimPrefix(server.URL, "http://")
						return server.Client().Do(r)
					}
				case "gin":
					require.Error(t, openrailsgin.Mount(gin.New(), a.engine, missing))
					require.Error(t, openrailsgin.Mount(gin.New(), a.engine, bad))
					app := gin.New()
					require.NoError(t, openrailsgin.Mount(app, a.engine, selection))
					server := httptest.NewServer(app)
					t.Cleanup(server.Close)
					request = func(r *http.Request) (*http.Response, error) {
						r.URL.Scheme = "http"
						r.URL.Host = strings.TrimPrefix(server.URL, "http://")
						return server.Client().Do(r)
					}
				case "fiber":
					require.Error(t, openrailsfiber.Mount(fiber.New(), a.engine, missing))
					require.Error(t, openrailsfiber.Mount(fiber.New(), a.engine, bad))
					app := fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true})
					require.NoError(t, openrailsfiber.Mount(app, a.engine, selection))
					t.Cleanup(func() { require.NoError(t, app.Shutdown()) })
					request = func(r *http.Request) (*http.Response, error) {
						return app.Test(r, fiber.TestConfig{Timeout: 10 * time.Second})
					}
				}
				for _, sample := range []struct {
					path, token string
					status      int
				}{
					{"/billing/v1/me", a.aliceToken, 200},
					{"/billing/v1/me", "", 401},
					{"/billing/v1/me", "invalid-credential", 401},
					{"/billing/v1/me/orders/" + order.ID.String(), a.aliceToken, 200},
					{"/billing/v1/me/orders/" + strings.Replace(order.ID.String(), "ord_", "%6Frd_", 1), a.aliceToken, 200},
					{"/billing/v1/me/orders/" + order.ID.String() + "%2Fextra", a.aliceToken, 404},
					{"/billing/v1/me/orders/" + order.ID.String(), a.bobToken, 404},
				} {
					r, err := http.NewRequestWithContext(t.Context(), "GET", "http://critical.invalid"+sample.path, nil)
					require.NoError(t, err)
					if sample.token != "" {
						r.Header.Set("Authorization", "Bearer "+sample.token)
					}
					response, err := request(r)
					require.NoError(t, err)
					raw, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, sample.status, response.StatusCode, sample.path+": "+string(raw))
					if sample.status == 200 {
						var body struct {
							ID string `json:"id"`
						}
						require.NoError(t, json.Unmarshal(raw, &body))
						if strings.HasSuffix(sample.path, "/me") {
							require.Equal(t, a.alice.ID, body.ID)
						} else {
							require.Equal(t, order.ID.String(), body.ID)
						}
					}
				}
			})
		}
	})
}
