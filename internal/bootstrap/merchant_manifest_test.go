package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseBootstrapManifest(t *testing.T) {
	// #527: a manifest is merchants-only. Each merchant carries its own inline
	// host-app issuer (registered as owner of its backing org), provider
	// accounts + secrets, and profile. No auth/users/orgs section.
	manifest, err := ParseBootstrapManifest([]byte(`
version: 1
merchants:
  - slug: cozy-art
    name: Cozy Art
    issuer:
      uri: https://auth.cozy.art
      jwks_uri: https://auth.cozy.art/.well-known/jwks.json
      audiences: [openrails]
    profile:
      display_name: Cozy Art Billing
      logo_url: https://cdn.example/logo.png
      from_email: billing@example.com
      support_url: https://example.com/support
      support_email: support@example.com
    provider_accounts:
      - provider_type: stripe
        account_id: acct_test_123
        role: primary
        secrets:
          secret_key:
            env: STRIPE_SECRET_KEY
      - provider_type: nmi
        account_id: mobius-profile-id
        secrets:
          webhook_signing_secret:
            env: MOBIUS_WEBHOOK_SIGNING_SECRET
`))
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 1)
	m := manifest.Merchants[0]
	require.Equal(t, "Cozy Art Billing", m.Profile.DisplayName)
	require.NotNil(t, m.Issuer)
	require.Equal(t, "https://auth.cozy.art", m.Issuer.URI)
	require.Equal(t, "https://auth.cozy.art/.well-known/jwks.json", m.Issuer.JWKSURI)
	require.Len(t, m.ProviderAccounts, 2)
	require.Equal(t, "acct_test_123", m.ProviderAccounts[0].AccountID)
}

func TestExampleBootstrapManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "merchants.example.yaml"))
	require.NoError(t, err)

	manifest, err := ParseBootstrapManifest(raw)
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 1)
	require.Equal(t, "local-stack", manifest.Merchants[0].Slug)
	require.NotNil(t, manifest.Merchants[0].Issuer)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", manifest.Merchants[0].Issuer.JWKSURI)
	require.Len(t, manifest.Merchants[0].ProviderAccounts, 4)
}

func TestParseBootstrapManifestValidationErrors(t *testing.T) {
	base := func(fragment string) string {
		return `
version: 1
merchants:
  - slug: cozy-art
    name: Cozy Art
` + fragment
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown top-level key",
			body: "version: 1\ntenantz: []\n",
			want: "tenantz",
		},
		{
			name: "auth section removed (hard cut)",
			body: "version: 1\nauth:\n  users: []\nmerchants:\n  - slug: x\n    name: X\n",
			want: "auth",
		},
		{
			name: "no merchants",
			body: "version: 1\n",
			want: "at least one merchant",
		},
		{
			name: "missing merchant name",
			body: "version: 1\nmerchants:\n  - slug: cozy-art\n",
			want: `merchant "cozy-art" name is required`,
		},
		{
			name: "issuer missing uri",
			body: base("    issuer:\n      jwks_uri: https://auth.cozy.art/.well-known/jwks.json\n"),
			want: "issuer.uri is required",
		},
		{
			name: "issuer both trust sources",
			body: base("    issuer:\n      uri: https://auth.cozy.art\n      jwks_uri: https://auth.cozy.art/jwks\n      public_keys:\n        - public_key_pem: x\n"),
			want: "exactly one of jwks_uri or public_keys",
		},
		{
			name: "issuer no trust source",
			body: base("    issuer:\n      uri: https://auth.cozy.art\n"),
			want: "must set jwks_uri or public_keys",
		},
		{
			name: "catalogs belong to push-merchant-catalog",
			body: "version: 1\ncatalogs: []\n",
			want: "catalogs",
		},
		{
			name: "invalid profile URL",
			body: base("    profile:\n      logo_url: ftp://cdn.example/logo.png\n"),
			want: "profile.logo_url",
		},
		{
			name: "invalid provider account role",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        account_id: acct_test_123\n        role: standby\n"),
			want: "role must be primary, secondary, or legacy",
		},
		{
			name: "invalid provider secret source",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        secrets:\n          secret_key:\n            value: one\n            env: STRIPE_SECRET_KEY\n"),
			want: "must set exactly one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBootstrapManifest([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
