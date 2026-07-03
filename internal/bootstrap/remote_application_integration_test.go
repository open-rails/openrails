//go:build integration

package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMerchantRemoteApplicationTrustSourcesIntegration(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	cfg := &config.Config{
		Env:  "test",
		DB:   &config.DBConfig{},
		Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
	}
	cp, err := controlplane.New(ctx, cfg, pool)
	require.NoError(t, err)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	jwksIssuer := "https://merchant-jwks-" + suffix + ".example"
	jwksURI := jwksIssuer + "/.well-known/jwks.json"
	_, err = ProvisionMerchant(ctx, ProvisionMerchantRequest{
		Config:       cfg,
		ControlPlane: cp,
		Database:     appDB,
		Slug:         "jwks-uri-" + suffix,
		Merchant: MerchantConfig{
			DisplayName: "JWKS URI Merchant",
			RemoteApplication: &RemoteApplicationConfig{
				Issuer:  jwksIssuer,
				JWKSURI: jwksURI,
			},
		},
		Options: MerchantManifestReconcileOptions{Insert: true},
	})
	require.NoError(t, err)

	storedJWKS, err := cp.Core().GetRemoteApplication(ctx, jwksIssuer)
	require.NoError(t, err)
	require.Equal(t, authkit.RemoteAppModeJWKS, storedJWKS.Mode)
	require.Equal(t, jwksURI, storedJWKS.JWKSURI)
	require.Empty(t, storedJWKS.PublicKeys)

	staticIssuer := "https://merchant-static-" + suffix + ".example"
	signer, err := jwtkit.NewEd25519Signer("static-ed25519-" + suffix)
	require.NoError(t, err)
	jwk := jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())
	staticSlug := "static-jwks-" + suffix
	merchantRow, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
		Config:       cfg,
		ControlPlane: cp,
		Database:     appDB,
		Slug:         staticSlug,
		Merchant: MerchantConfig{
			DisplayName: "Static JWKS Merchant",
			RemoteApplication: &RemoteApplicationConfig{
				Issuer: staticIssuer,
				JWKS: StaticJWKSConfig{Keys: []StaticJWKConfig{{
					Kty: jwk.Kty,
					Use: jwk.Use,
					Kid: jwk.Kid,
					Alg: jwk.Alg,
					Crv: jwk.Crv,
					X:   jwk.X,
				}}},
			},
		},
		Options: MerchantManifestReconcileOptions{Insert: true},
	})
	require.NoError(t, err)

	storedStatic, err := cp.Core().GetRemoteApplication(ctx, staticIssuer)
	require.NoError(t, err)
	require.Equal(t, authkit.RemoteAppModeStatic, storedStatic.Mode)
	require.Empty(t, storedStatic.JWKSURI)
	require.Len(t, storedStatic.PublicKeys, 1)
	require.Equal(t, signer.KID(), storedStatic.PublicKeys[0].KID)
	require.Contains(t, storedStatic.PublicKeys[0].PublicKeyPEM, "BEGIN PUBLIC KEY")

	token, err := authcore.MintRemoteApplicationAccessToken(ctx, signer, authkit.RemoteApplicationAccessParams{
		Issuer:    staticIssuer,
		Audiences: []string{"openrails"},
		TTL:       time.Hour,
	})
	require.NoError(t, err)

	resolved, err := cp.ResolveRemoteApplication(ctx, token)
	require.NoError(t, err)
	require.Equal(t, staticSlug, resolved.MerchantSlug)
	require.Equal(t, merchantRow.ID, resolved.MerchantID)
	require.True(t, resolved.HasPermission(controlplane.PermMerchantCatalogUpdate))
}
