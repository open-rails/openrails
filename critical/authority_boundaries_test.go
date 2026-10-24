//go:build integration

package critical_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestAuthorityBoundaries(t *testing.T) {
	t.Run("OR-J01-S01/merchant-and-root-authority-read-owned-state", func(t *testing.T) {
		a := newAuthorityWorld(t)
		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "owned", DisplayName: "Owned product"})
		require.NoError(t, err)
		authtest.GrantRole(t, a.auth, iam.RootGroup(), iam.UserSubject(a.alice.ID), iam.RootPersona().OwnerRole())
		root := a.remote(t, a.alpha.MerchantID(), a.aliceToken)
		read, err := root.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, product.ID, read.ID)
		require.Equal(t, "Owned product", read.DisplayName)
		merchant, err := a.alphaRemote.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, read, merchant)
	})
	t.Run("OR-J01-S02/missing-and-malformed-credential-create-nothing", func(t *testing.T) {
		a := newAuthorityWorld(t)
		rawContract, err := os.ReadFile("http_contract.json")
		require.NoError(t, err)
		var contract struct {
			Operations []struct{ Method, Path, Auth, Feature string }
		}
		require.NoError(t, json.Unmarshal(rawContract, &contract))
		routes, err := a.engine.Routes(openrails.Routes{Auth: a.auth.Authenticator(), RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}})
		require.NoError(t, err)
		mounted := map[string]bool{}
		for _, route := range routes {
			mounted[route.Method+" "+route.Path] = true
		}
		parameter := regexp.MustCompile(`\{[^}]+\}`)
		checked, mandatory := 0, 0
		for _, operation := range contract.Operations {
			if operation.Auth == "public" || operation.Auth == "provider_signature" {
				continue
			}
			key := operation.Method + " " + operation.Path
			if operation.Feature == "" {
				mandatory++
			}
			if !mounted[key] {
				require.NotEmpty(t, operation.Feature, "mandatory protected operation was not mounted: %s", key)
				continue // Enabled optional profiles own their additional gate probes.
			}
			checked++
			path := parameter.ReplaceAllString(operation.Path, "11111111-1111-4111-8111-111111111111")
			for _, credential := range []string{"", "not-a-valid-credential"} {
				status, raw := a.request(t, operation.Method, path, credential, map[string]any{})
				require.Equal(t, http.StatusUnauthorized, status, key+": "+string(raw))
			}
		}
		require.NotZero(t, mandatory, "the reviewed contract must include the protected core")
		require.GreaterOrEqual(t, checked, mandatory, "every mandatory protected operation must be exercised")
		t.Logf("exercised missing and malformed credentials on %d reviewed mounted operations (%d mandatory core operations)", checked, mandatory)
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Empty(t, products.Items)
	})
	t.Run("OR-J01-S03/expired-and-revoked-credentials-fail-closed", func(t *testing.T) {
		a := newAuthorityWorld(t)
		expired := a.signedToken(t, func(_ map[string]any, c map[string]any) {
			c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
			c["iat"] = time.Now().Add(-3 * time.Hour).Unix()
			c["nbf"] = time.Now().Add(-3 * time.Hour).Unix()
		})
		status, raw := a.request(t, "GET", "/v1/me", expired, nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		require.Contains(t, string(raw), `"code":"credential_expired"`)
		_, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err, "credential must work before revocation")
		require.NoError(t, a.auth.RevokeAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, a.alphaKey.APIKey.ID))
		_, err = a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "after-revoke", DisplayName: "Refused"})
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, 401, refusal.Status)
		require.Equal(t, "credential_revoked", refusal.Code)
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Empty(t, products.Items)
	})
	t.Run("OR-J01-S04/issuer-audience-signature-and-sender-proof", func(t *testing.T) {
		a := newAuthorityWorld(t)
		authtest.GrantRole(t, a.auth, iam.RootGroup(), iam.UserSubject(a.alice.ID), iam.RootPersona().OwnerRole())
		root := a.remote(t, a.alpha.MerchantID(), a.aliceToken)
		_, err := root.CreateProduct(t.Context(), billing.CreateProductParams{Key: "valid-control", DisplayName: "Valid"})
		require.NoError(t, err)
		mutations := []struct {
			name   string
			change func(map[string]any, map[string]any)
		}{
			{"issuer", func(_ map[string]any, c map[string]any) { c["iss"] = "https://other-issuer.invalid" }},
			{"audience", func(_ map[string]any, c map[string]any) { c["aud"] = []string{"other-service"} }},
			{"key-id", func(h map[string]any, _ map[string]any) { h["kid"] = "not-a-trusted-key" }},
			{"sender-proof", func(_ map[string]any, c map[string]any) {
				c["cnf"] = map[string]any{"jkt": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
			}},
		}
		for _, mutation := range mutations {
			token := a.signedToken(t, mutation.change)
			status, raw := a.request(t, "POST", "/v1/catalog/products", token, billing.CreateProductParams{Key: mutation.name, DisplayName: "Refused"})
			require.Equal(t, http.StatusUnauthorized, status, mutation.name+": "+string(raw))
		}
		parts := strings.Split(a.aliceToken, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		require.NoError(t, err)
		signature[0] ^= 0x80
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)
		status, raw := a.request(t, "POST", "/v1/catalog/products", strings.Join(parts, "."), billing.CreateProductParams{Key: "bad-signature", DisplayName: "Refused"})
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		require.Equal(t, "valid-control", products.Items[0].Key)
	})
	t.Run("OR-J01-S05/correct-permission-in-wrong-merchant", func(t *testing.T) {
		a := newAuthorityWorld(t)
		wrong := a.remote(t, a.alpha.MerchantID(), a.betaKey.Secret)
		_, err := wrong.CreateProduct(t.Context(), billing.CreateProductParams{Key: "misdirected", DisplayName: "Refused"})
		var mismatch *openrails.BoundMerchantError
		require.ErrorAs(t, err, &mismatch)
		require.Equal(t, a.alpha.MerchantID(), mismatch.Merchant)
		require.Equal(t, a.beta.MerchantID(), mismatch.Bound)
		status, raw := a.request(t, "POST", "/v1/catalog/products", a.betaKey.Secret, billing.CreateProductParams{Key: "wire-misdirected", DisplayName: "Refused"}, map[string]string{"OpenRails-Expected-Merchant": a.alpha.MerchantID().String()})
		require.Equal(t, 412, status, string(raw))
		require.Contains(t, string(raw), `"code":"merchant_mismatch"`)
		for _, client := range []*openrails.Client{a.alpha, a.beta} {
			page, err := client.ListProducts(t.Context(), billing.ProductListParams{})
			require.NoError(t, err)
			require.Empty(t, page.Items)
		}
		_, err = a.betaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "correct-control", DisplayName: "Beta"})
		require.NoError(t, err)
	})
	t.Run("OR-J01-S07/foreign-resource-detail-list-batch-and-cursor", func(t *testing.T) {
		a := newAuthorityWorld(t)
		for _, key := range []string{"alpha-secret-one", "alpha-secret-two"} {
			_, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Alpha private"})
			require.NoError(t, err)
		}
		alpha, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{PageRequest: billing.PageRequest{Limit: 1}})
		require.NoError(t, err)
		require.Len(t, alpha.Items, 1)
		require.NotEmpty(t, alpha.Next)
		beta, err := a.beta.CreateProduct(t.Context(), billing.CreateProductParams{Key: "beta-control", DisplayName: "Beta"})
		require.NoError(t, err)
		_, err = a.betaRemote.GetProduct(t.Context(), alpha.Items[0].ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		batch, err := a.betaRemote.ListProducts(t.Context(), billing.ProductListParams{IDs: []billing.ProductID{alpha.Items[0].ID, beta.ID}})
		require.NoError(t, err)
		require.Len(t, batch.Items, 1)
		require.Equal(t, beta.ID, batch.Items[0].ID)
		page, err := a.betaRemote.ListProducts(t.Context(), billing.ProductListParams{PageRequest: billing.PageRequest{Cursor: alpha.Next}})
		require.NoError(t, err)
		for _, item := range page.Items {
			require.Equal(t, beta.ID, item.ID)
		}
		all, err := a.betaRemote.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, all.Items, 1)
		require.Equal(t, beta.ID, all.Items[0].ID)
	})
	t.Run("OR-J01-S08/real-authority-storage-failure-denies-mutations", func(t *testing.T) {
		a := newAuthorityWorld(t)
		_, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "before-outage", DisplayName: "Control"})
		require.NoError(t, err)
		// A new credential has never been resolved. The earlier valid call is
		// a positive control for this real issuer and route before its storage fails.
		cold, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "cold-outage-key", Role: a.merchant.Owner})
		require.NoError(t, err)
		// Cut existing and future AuthKit TCP connections. Its owned schema pool
		// cloned this DSN; closing only the input pool would not create an outage.
		a.storage.cut()
		status, raw := a.request(t, "POST", "/v1/catalog/products", cold.Secret, billing.CreateProductParams{Key: "outage-write", DisplayName: "Refused"})
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, products.Items, 1, "an authority outage must not authorize a mutation")
		require.Equal(t, "before-outage", products.Items[0].Key)
		require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
		require.Contains(t, string(raw), `"code":"authentication_unavailable"`)
	})

}
