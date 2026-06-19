package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMerchantConfigManifest(t *testing.T) {
	// #527: a manifest is merchants-only. Each merchant carries its own inline
	// host-app issuer (registered as owner of its backing org), provider
	// accounts + secrets, and profile. No auth/users/orgs section.
	manifest, err := ParseMerchantConfigManifest([]byte(`
version: 1
merchants:
  - slug: cozy-art
    display_name: Cozy Art
    issuer:
      uri: https://auth.cozy.art
      jwks_uri: https://auth.cozy.art/.well-known/jwks.json
      audiences: [openrails]
    profile:
      display_name: Cozy Art Billing
      logo_url: https://cdn.example/logo.png
      from_email: billing@example.com
      support_url: https://example.com/support
    provider_accounts:
      - provider_type: stripe
        environment: test
        account_id: acct_test_123
        mode: primary
        secrets:
          secret_key:
            env: STRIPE_SECRET_KEY
      - provider_type: nmi
        environment: live
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

func TestExampleMerchantConfigManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "merchants.example.yaml"))
	require.NoError(t, err)

	manifest, err := ParseMerchantConfigManifest(raw)
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 1)
	require.Equal(t, "local-stack", manifest.Merchants[0].Slug)
	require.NotNil(t, manifest.Merchants[0].Issuer)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", manifest.Merchants[0].Issuer.JWKSURI)
	require.Len(t, manifest.Merchants[0].ProviderAccounts, 4)
}

func TestParseBootstrapManifest(t *testing.T) {
	raw := []byte(`
version: 1
authority:
  bootstrap_org_slug: local-stack
  initial_admin_user_id: usr_admin
  mint_initial_service_token: false
`)
	manifest, err := ParseBootstrapManifest(raw)
	require.NoError(t, err)

	opts := manifest.BootstrapOptions()
	require.Equal(t, "local-stack", opts.BootstrapOrgSlug)
	require.Equal(t, "usr_admin", opts.InitialAdminUserID)
	require.False(t, opts.MintInitialServiceToken)
}

func TestExampleBootstrapManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)

	manifest, err := ParseBootstrapManifest(raw)
	require.NoError(t, err)
	require.Equal(t, "local-stack", manifest.BootstrapOptions().BootstrapOrgSlug)
}

func TestParseBootstrapManifestValidationErrors(t *testing.T) {
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
			name: "merchant config belongs elsewhere",
			body: "version: 1\nmerchants: []\n",
			want: "merchants",
		},
		{
			name: "catalog belongs elsewhere",
			body: "version: 1\ncatalogs: []\n",
			want: "catalogs",
		},
		{
			name: "missing authority",
			body: "version: 1\n",
			want: "authority.bootstrap_org_slug",
		},
		{
			name: "missing bootstrap org slug",
			body: "version: 1\nauthority: {}\n",
			want: "authority.bootstrap_org_slug",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBootstrapManifest([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestParseMerchantConfigManifestValidationErrors(t *testing.T) {
	base := func(fragment string) string {
		return `
version: 1
merchants:
  - slug: cozy-art
    display_name: Cozy Art
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
			body: "version: 1\nauth:\n  users: []\nmerchants:\n  - slug: x\n    display_name: X\n",
			want: "auth",
		},
		{
			name: "bootstrap authority belongs to push-bootstrap",
			body: "version: 1\nauthority:\n  bootstrap_org_slug: local-stack\n",
			want: "authority",
		},
		{
			name: "no merchants",
			body: "version: 1\n",
			want: "at least one merchant",
		},
		{
			name: "missing merchant display name",
			body: "version: 1\nmerchants:\n  - slug: cozy-art\n",
			want: `merchant "cozy-art" display_name is required`,
		},
		{
			name: "merchant name removed",
			body: "version: 1\nmerchants:\n  - slug: cozy-art\n    name: Cozy Art\n",
			want: "unknown field \"name\"",
		},
		{
			name: "support email removed",
			body: base("    profile:\n      support_email: support@example.com\n"),
			want: "support_email",
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
			name: "invalid provider account mode",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        account_id: acct_test_123\n        mode: standby\n"),
			want: "mode must be primary, secondary, legacy, or disabled",
		},
		{
			name: "provider account role removed",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        account_id: acct_test_123\n        role: primary\n"),
			want: "unknown field \"role\"",
		},
		{
			name: "invalid provider account environment",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        environment: moon\n        account_id: acct_test_123\n"),
			want: "environment must be live or test",
		},
		{
			name: "invalid provider secret source",
			body: base("    provider_accounts:\n      - provider_type: stripe\n        secrets:\n          secret_key:\n            value: one\n            env: STRIPE_SECRET_KEY\n"),
			want: "must set exactly one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
