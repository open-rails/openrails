package bootstrap

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	akembedded "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"
)

type fakeTransit struct {
	pub []byte
	err error
}

func (f fakeTransit) Sign(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (f fakeTransit) PublicKey(context.Context, string) ([]byte, error)    { return f.pub, f.err }

func TestParseMerchantConfigManifest(t *testing.T) {
	// #527: a manifest is merchants-only. Each merchant carries its own inline
	// host-app remote_application (registered as owner of its permission-group),
	// provider accounts + secrets, and profile. No auth/users/groups section.
	manifest, err := ParseMerchantConfigManifest([]byte(`
version: 1
merchants:
  cozy-art:
    display_name: Cozy Art
    remote_application:
      issuer: https://auth.cozy.art
      jwks_uri: https://auth.cozy.art/.well-known/jwks.json
    profile:
      display_name: Cozy Art Billing
      logo_url: https://cdn.example/logo.png
      from_email: billing@example.com
      support_url: https://example.com/support
    accounts:
      stripe:
        stripe:
          environment: test
          account_id: acct_test_123
          secrets:
            secret_key: sk_test_123
      mobius:
        nmi:
          environment: live
          account_id: mobius-profile-id
          settings:
            tokenization_url: https://secure.networkmerchants.com/token/Collect.js
            tokenization_key: public-tokenization-key
          secrets:
            webhook_signing_secret: mobius-webhook-secret
`))
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 1)
	m := manifest.Merchants["cozy-art"]
	require.Equal(t, "Cozy Art Billing", m.Profile.DisplayName)
	require.NotNil(t, m.RemoteApplication)
	require.Equal(t, "https://auth.cozy.art", m.RemoteApplication.Issuer)
	require.Equal(t, "https://auth.cozy.art/.well-known/jwks.json", m.RemoteApplication.JWKSURI)
	require.Len(t, m.RailMerchantAccounts, 2)
	require.Equal(t, "acct_test_123", m.RailMerchantAccounts["stripe"]["stripe"].AccountID)
}

func TestExampleMerchantConfigManifestParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "merchants_config.example.yaml"))
	require.NoError(t, err)

	manifest, err := ParseMerchantConfigManifest(raw)
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 2)
	m := manifest.Merchants["local-stack"]
	require.NotNil(t, m.RemoteApplication)
	require.Equal(t, "https://local-stack.example", m.RemoteApplication.Issuer)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", m.RemoteApplication.JWKSURI)

	staticMerchant := manifest.Merchants["static-jwks-stack"]
	require.NotNil(t, staticMerchant.RemoteApplication)
	require.Equal(t, "https://static-jwks.example", staticMerchant.RemoteApplication.Issuer)
	require.Len(t, staticMerchant.RemoteApplication.JWKS.Keys, 1)
	require.Equal(t, "static-ed25519-1", staticMerchant.RemoteApplication.JWKS.Keys[0].Kid)

	// #646: the example carries the COMPLETE merchant_configurations payload.
	require.NotNil(t, m.Invoice)
	require.Equal(t, "calendar_month", m.Invoice.BillingPeriodBoundary)
	require.NotNil(t, m.Invoice.CollectionThreshold)
	require.Equal(t, int64(50_000_000), *m.Invoice.CollectionThreshold)
	require.NotNil(t, m.Invoice.MonthlyFloor)
	require.Equal(t, int64(1_000_000), *m.Invoice.MonthlyFloor)

	require.Len(t, m.DelegatedInvokerWastedSpendWindows, 2)
	require.Equal(t, "burst", m.DelegatedInvokerWastedSpendWindows[0].Key)
	require.Equal(t, "15m", m.DelegatedInvokerWastedSpendWindows[0].Window)
	require.Equal(t, int64(5_000_000), m.DelegatedInvokerWastedSpendWindows[0].Limit)

	// #641/#646/#655/#660: multiple accounts per rail, each with a human name,
	// account_id identity, lifecycle, and explicit signer/destination split for
	// Solana.
	accts := m.RailMerchantAccounts
	require.Len(t, accts, 8)
	type key struct{ name, env string }
	byName := map[key]ProviderRailAccountConfig{}
	byRail := map[string]string{}
	for name, account := range accts {
		require.Len(t, account, 1)
		for rail, cfg := range account {
			byName[key{name, cfg.Environment}] = cfg
			byRail[name] = rail
		}
	}
	// NMI gateway "mobius": live gateway-id + its sandbox.
	require.Equal(t, "nmi", byRail["mobius"])
	require.Equal(t, "579145", byName[key{"mobius", "live"}].AccountID)
	require.False(t, byName[key{"mobius", "live"}].Archived)
	require.Equal(t, "replace-with-live-nmi-tokenization-key", byName[key{"mobius", "live"}].Settings["tokenization_key"])
	require.Equal(t, "681902", byName[key{"mobius-sandbox", "test"}].AccountID)
	// A second NMI gateway (paykings) is archived/drain-only in the example.
	require.True(t, byName[key{"paykings", "live"}].Archived)
	// Stripe live + test side by side.
	require.Equal(t, "acct_1M9QZULkdIwHu7ix", byName[key{"stripe", "live"}].AccountID)
	require.Equal(t, "acct_1N2YbMLkdIwHu7ix", byName[key{"stripe-sandbox", "test"}].AccountID)
	// CCBill — one account.
	require.Equal(t, "945280-0000", byName[key{"ccbill", "live"}].AccountID)
}

func TestExampleAuthKitAuthorityManifestParses(t *testing.T) {
	_, err := akembedded.LoadBootstrapManifestFile(filepath.Join("..", "..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)
}

func TestManifestSolanaSignerEvidence(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	accountID := solanago.PublicKeyFromBytes(pub).String()
	exampleAccountID := "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9"
	examplePrivateKey := "2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT"
	secrets, err := newManifestSecretValues("solana", map[string]string{
		"private_key": examplePrivateKey,
	})
	require.NoError(t, err)

	got, gotAccountID, err := manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{}, secrets, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"mode": "local_keypair"}, got)
	require.Equal(t, exampleAccountID, gotAccountID)

	// A declared account_id is IGNORED (warned), never an error — derived from the key.
	_, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", exampleAccountID, ProviderRailAccountConfig{
		Signer: &RailMerchantAccountSignerConfig{Mode: "local_keypair"},
	}, secrets, nil)
	require.NoError(t, err)
	require.Equal(t, exampleAccountID, gotAccountID, "declared account_id ignored; derived from the keypair")

	_, _, err = manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{
		Signer: &RailMerchantAccountSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
	}, secrets, fakeTransit{pub: pub})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot also set secrets.private_key")

	emptySecrets, err := newManifestSecretValues("solana", nil)
	require.NoError(t, err)
	// vault_transit with a declared account_id: ignored (warned); derives from the Transit key.
	_, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", accountID, ProviderRailAccountConfig{
		Signer: &RailMerchantAccountSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
	}, emptySecrets, fakeTransit{pub: pub})
	require.NoError(t, err)
	require.Equal(t, accountID, gotAccountID, "declared account_id ignored; derived from the Transit key")

	got, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{
		Signer: &RailMerchantAccountSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
	}, emptySecrets, fakeTransit{pub: pub})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"mode": "vault_transit", "key": "openrails-solana-local"}, got)
	require.Equal(t, accountID, gotAccountID)

	// Missing signer/key remains empty here; manifest validation rejects it earlier.
	_, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{}, emptySecrets, nil)
	require.NoError(t, err)
	require.Empty(t, gotAccountID, "receive-only account has no account_id")
}

func TestParseMerchantConfigManifestValidationErrors(t *testing.T) {
	base := func(fragment string) string {
		return `
version: 1
merchants:
  cozy-art:
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
			body: "version: 1\nauth:\n  users: []\nmerchants:\n  x:\n    display_name: X\n",
			want: "auth",
		},
		{
			name: "authkit authority belongs elsewhere",
			body: "users:\n  - username: operator\n",
			want: "users",
		},
		{
			name: "no merchants",
			body: "version: 1\n",
			want: "at least one merchant",
		},
		{
			name: "missing merchant display name",
			body: "version: 1\nmerchants:\n  cozy-art: {}\n",
			want: `merchant "cozy-art" display_name is required`,
		},
		{
			name: "merchant name removed",
			body: "version: 1\nmerchants:\n  cozy-art:\n    name: Cozy Art\n",
			want: "unknown field \"name\"",
		},
		{
			name: "support email removed",
			body: base("    profile:\n      support_email: support@example.com\n"),
			want: "support_email",
		},
		{
			name: "issuer section removed",
			body: base("    issuer:\n      issuer: https://auth.cozy.art\n      jwks_uri: https://auth.cozy.art/.well-known/jwks.json\n"),
			want: "issuer",
		},
		{
			name: "remote application missing issuer",
			body: base("    remote_application:\n      jwks_uri: https://auth.cozy.art/.well-known/jwks.json\n"),
			want: "remote_application.issuer is required",
		},
		{
			name: "remote application both trust sources",
			body: base("    remote_application:\n      issuer: https://auth.cozy.art\n      jwks_uri: https://auth.cozy.art/jwks\n      public_keys:\n        - public_key_pem: x\n"),
			want: "exactly one of jwks_uri, jwks, or public_keys",
		},
		{
			name: "remote application no trust source",
			body: base("    remote_application:\n      issuer: https://auth.cozy.art\n"),
			want: "must set jwks_uri, jwks, or public_keys",
		},
		{
			name: "remote application allowed origins removed",
			body: base("    remote_application:\n      issuer: https://auth.cozy.art\n      jwks_uri: https://auth.cozy.art/jwks\n      allowed_origins:\n        - https://auth.cozy.art\n"),
			want: "allowed_origins",
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
			name: "renamed key rail_merchant_accounts rejected with pointer (#698)",
			body: base("    rail_merchant_accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n"),
			want: "merchants.cozy-art.rail_merchant_accounts was renamed to accounts",
		},
		{
			name: "pre-#683 key provider_accounts rejected with pointer",
			body: base("    provider_accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n"),
			want: "merchants.cozy-art.provider_accounts was renamed to accounts",
		},
		{
			name: "provider account routing removed",
			body: base("    accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          routing: standby\n"),
			want: "unknown field \"routing\"",
		},
		{
			name: "provider account mode removed",
			body: base("    accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          mode: primary\n"),
			want: "unknown field \"mode\"",
		},
		{
			name: "provider account role removed",
			body: base("    accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          role: primary\n"),
			want: "unknown field \"role\"",
		},
		{
			name: "invalid provider account environment",
			body: base("    accounts:\n      stripe:\n        stripe:\n          environment: moon\n          account_id: acct_test_123\n"),
			want: "environment must be live or test",
		},
		{
			name: "solana network is not a provider-account knob",
			body: base("    accounts:\n      solana:\n        solana:\n          network: devnet\n"),
			want: "unknown field \"network\"",
		},
		{
			name: "invalid provider secret alias",
			body: base("    accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          secrets:\n            api_key: one\n"),
			want: "unknown provider account secret",
		},
		{
			name: "nmi tokenization key is a setting",
			body: base("    accounts:\n      mobius:\n        nmi:\n          account_id: mobius-profile-id\n          secrets:\n            tokenization_key: public-token\n"),
			want: "unknown provider account secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// #698: the dump emits the renamed `accounts:` key and round-trips through the
// strict parser. The DB table / secret-name prefix keep `rail_merchant_accounts`
// on purpose — only the config surface is short.
func TestMarshalMerchantManifestEmitsAccountsKey(t *testing.T) {
	encoded, err := MarshalMerchantManifest(&BillingConfig{
		Version: 1,
		Merchants: map[string]MerchantConfig{
			"cozy-art": {
				DisplayName: "Cozy Art",
				RailMerchantAccounts: map[string]RailMerchantAccountConfig{
					"stripe": {"stripe": {Environment: "test", AccountID: "acct_test_123"}},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Contains(t, string(encoded), "accounts:")
	require.NotContains(t, string(encoded), "rail_merchant_accounts:")

	reparsed, err := ParseMerchantConfigManifest(encoded)
	require.NoError(t, err)
	require.Equal(t, "acct_test_123", reparsed.Merchants["cozy-art"].RailMerchantAccounts["stripe"]["stripe"].AccountID)
}

// A declared Solana account_id is IGNORED, not rejected: parsing succeeds (it is
// derived from the signer at apply, with a warning). A Solana account with no
// signer to derive from is the only Solana parse error.
func TestParseMerchantConfigManifestSolanaAccountIDIgnored(t *testing.T) {
	base := func(fragment string) string {
		return "version: 1\nmerchants:\n  cozy-art:\n    display_name: Cozy Art\n" + fragment
	}
	withAccountID := base("    accounts:\n      solana:\n        solana:\n          account_id: AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9\n          signer: { mode: local_keypair }\n          secrets:\n            private_key: 2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT\n")
	_, err := ParseMerchantConfigManifest([]byte(withAccountID))
	require.NoError(t, err, "declared solana account_id is ignored, not a parse error")

	noSigner := base("    accounts:\n      solana:\n        solana:\n          environment: live\n")
	_, err = ParseMerchantConfigManifest([]byte(noSigner))
	require.ErrorContains(t, err, "requires a signer", "solana with no signer has no key to derive account_id from")
}
