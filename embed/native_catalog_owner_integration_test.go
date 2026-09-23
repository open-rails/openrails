//go:build integration

package embed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func TestNativeCatalogOwnerSelectionRequiresCanonicalSelfOrLiveAdmin(t *testing.T) {
	ctx := t.Context()
	admin, pool, dsn := scopeWithoutRLSDatabase(t)
	slug := "native-catalog-" + uuid.NewString()
	channel, customer := uuid.NewString(), uuid.NewString()
	const staffSubject = "opaque-staff-subject"
	const issuer = "https://catalog-identity.test"
	secret := []byte("native-catalog-owner-fixture-signing-key")
	issue := func(subject string) string {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": subject, "iss": issuer, "aud": "billing", "exp": time.Now().Add(time.Hour).Unix(),
		}).SignedString(secret)
		require.NoError(t, err)
		return token
	}
	staffToken, customerToken := issue(staffSubject), issue("mapped-customer")
	var adminAllowed atomic.Bool
	auth := &billingauth.Integration{
		Authentication: billingauth.AuthenticationFunc(func(_ context.Context, r *http.Request) (billingauth.Identity, error) {
			parsed, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithAudience("billing"))
			if err != nil || !parsed.Valid {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
			subject, _ := parsed.Claims.GetSubject()
			identity := billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: subject, Issuer: issuer, CredentialClass: billingauth.CredentialClassUserSession}
			if subject == "mapped-customer" {
				identity.CustomerID = customer
			}
			return identity, nil
		}),
		Authorization: billingauth.AuthorizationFunc(func(_ context.Context, _ *http.Request, identity billingauth.Identity, q billingauth.Requirement) error {
			if q.Scope != billingauth.MerchantScope || q.Target.MerchantID.IsZero() || q.Target.MerchantSlug != slug {
				return billingauth.GateError{Status: 403, Message: "wrong merchant target"}
			}
			switch q.Permission {
			case permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate:
				return nil
			case permissions.MerchantCatalogRead, permissions.MerchantCatalogUpdate:
				if identity.SubjectID == staffSubject && adminAllowed.Load() {
					return nil
				}
			}
			return billingauth.GateError{Status: 403, Message: "catalog administrator permission denied"}
		}),
	}
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{
		Config: &config.Config{
			Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
			AllowCatalogUpdates: true, ProviderWriteMode: config.ProviderWriteModeReadOnly,
			DB: &config.DBConfig{URL: dsn},
		},
		PGXPool: pool, River: embed.RiverFromHost(), Auth: auth, HTTP: &embed.HTTPConfig{Catalog: true},
	}, slug, embed.MerchantConfig{DisplayName: "Native catalog"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	routes, err := runtime.HTTPRoutes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ownerOf := func(product *openrails.Product) string {
		t.Helper()
		id, err := openrails.ParseProductID(product.ID)
		require.NoError(t, err)
		var owner string
		require.NoError(t, admin.QueryRow(ctx, "SELECT c.owner_subject FROM billing.products p JOIN billing.catalogs c ON c.id=p.catalog_id AND c.merchant_id=p.merchant_id WHERE p.id=$1 AND p.merchant_id=$2", id.UUID(), mid.UUID()).Scan(&owner))
		return owner
	}
	for _, mode := range []struct {
		name   string
		client func(string) (*openrails.Client, error)
	}{
		{"headless", func(token string) (*openrails.Client, error) {
			return runtime.Client(openrails.WithAPIKey(token), openrails.WithOwnCatalog())
		}},
		{"http", func(token string) (*openrails.Client, error) {
			return openrails.NewRemote(server.URL, openrails.WithAPIKey(token), openrails.WithOwnCatalog())
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			selected := openrails.WithMerchant(slug)
			adminAllowed.Store(false)
			staff, err := mode.client(staffToken)
			require.NoError(t, err)
			_, err = staff.Products.List(ctx, nil, selected)
			require.ErrorIs(t, err, openrails.ErrDenied, "unmapped staff cannot obtain a personal catalog from an opaque subject")
			channelClient, err := staff.ForCatalogOwner(channel)
			require.NoError(t, err)
			_, err = channelClient.Products.Create(ctx, &openrails.ProductCreateParams{Key: "denied-" + mode.name, DisplayName: "Denied"}, selected)
			require.ErrorIs(t, err, openrails.ErrDenied)
			rawSubjectClient, err := staff.ForCatalogOwner(staffSubject)
			require.NoError(t, err)
			_, err = rawSubjectClient.Products.List(ctx, nil, selected)
			require.ErrorIs(t, err, openrails.ErrDenied, "matching raw opaque subject must not bypass the administrator check")
			adminAllowed.Store(true)
			product, err := channelClient.Products.Create(ctx, &openrails.ProductCreateParams{Key: "channel-" + mode.name, DisplayName: "Channel product"}, selected)
			require.NoError(t, err)
			require.Equal(t, channel, ownerOf(product), "channel is the selected catalog owner, not a fabricated customer mapping")
			_, err = channelClient.Products.Retrieve(ctx, product.ID, selected)
			require.NoError(t, err)
			adminAllowed.Store(false)
			_, err = channelClient.Products.Retrieve(ctx, product.ID, selected)
			require.ErrorIs(t, err, openrails.ErrDenied, "same client and token must observe revoked administrator authority")
			personal, err := mode.client(customerToken)
			require.NoError(t, err)
			personalProduct, err := personal.Products.Create(ctx, &openrails.ProductCreateParams{Key: "personal-" + mode.name, DisplayName: "Personal product"}, selected)
			require.NoError(t, err)
			require.Equal(t, customer, ownerOf(personalProduct))
			otherOwner, err := personal.ForCatalogOwner(channel)
			require.NoError(t, err)
			_, err = otherOwner.Products.Retrieve(ctx, product.ID, selected)
			require.ErrorIs(t, err, openrails.ErrDenied, "canonical personal ownership grants no channel authority")
		})
	}
}
