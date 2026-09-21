package bootstrap

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	akembedded "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/stretchr/testify/require"
)

type fakeTransit struct {
	pub []byte
	err error
}

func (f fakeTransit) Sign(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (f fakeTransit) PublicKey(context.Context, string) ([]byte, error)    { return f.pub, f.err }

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
	accts := m.PSPs
	require.Len(t, accts, 10)
	byName := map[string]ProviderRailAccountConfig{}
	byRail := map[string]string{}
	for name, account := range accts {
		require.Len(t, account, 1)
		for rail, cfg := range account {
			// #882: the example declares no `environment:` — it is derived.
			require.Empty(t, cfg.LegacyEnvironment)
			byName[name] = cfg
			byRail[name] = rail
		}
	}
	// NMI gateway "mobius" plus a second account on the same rail.
	require.Equal(t, "nmi", byRail["mobius"])
	require.Equal(t, "1234567", byName["mobius"].AccountID)
	require.False(t, byName["mobius"].Archived)
	require.Equal(t, "replace-with-live-nmi-tokenization-key", byName["mobius"].Settings["tokenization_key"])
	require.Equal(t, "7654321", byName["mobius-sandbox"].AccountID)

	// or#879/or#880 custody: NMI PSPs whose cards are held by Basis Theory. The
	// rail is nmi (it always was); the custodian is declared ONCE and both
	// gateways reference it by key.
	require.Equal(t, "nmi", byRail["mobius-bt"])
	require.Equal(t, "7654322", byName["mobius-bt"].AccountID)
	require.Equal(t, "bt", byName["mobius-bt"].Custodian)
	require.Equal(t, "bt", byName["mobius-bt-backup"].Custodian)
	require.Empty(t, byName["mobius-bt"].Settings, "custody is a reference now — nothing custodial belongs in PSP settings")

	// The custodian block itself: one entry, keyed by vendor kind, carrying the
	// tenant identity, the public checkout key and the ONE custodial secret.
	require.Len(t, m.Custodians, 1)
	btKinds := m.Custodians["bt"]
	require.Len(t, btKinds, 1)
	bt, ok := btKinds[models.CustodianBasisTheory]
	require.True(t, ok, "the custodian entry is keyed by its vendor kind")
	require.Equal(t, "replace-with-bt-tenant-id", bt.AccountID)
	require.Equal(t, "replace-with-bt-public-application-key", bt.Settings[custodians.SettingPublicAPIKey])
	require.Equal(t, "replace-with-bt-private-application-key", bt.Secrets[custodians.SecretAPIKey])
	require.NoError(t, config.ValidateCustodianEntry(config.CustodianEntry{
		Key: "bt", Kind: models.CustodianBasisTheory, AccountID: bt.AccountID,
		Settings: bt.Settings, SecretKeys: []string{custodians.SecretAPIKey},
	}))
	// A second NMI gateway (paykings) is archived/drain-only in the example.
	require.True(t, byName["paykings"].Archived)
	// Two Stripe accounts side by side.
	require.Equal(t, "acct_1AbCdEfGhIjKlMnOp", byName["stripe"].AccountID)
	require.Equal(t, "acct_1ZyXwVuTsRqPoNmL", byName["stripe-sandbox"].AccountID)
	// CCBill — one account.
	require.Equal(t, "999999-0000", byName["ccbill"].AccountID)

	// #711: the example's solana settings block carries the runtime knobs and
	// passes the strict push-time validation.
	solanaSettings := byName["solana"].Settings
	require.NoError(t, config.ValidateSolanaAccountSettings(solanaSettings))
	parsed, err := config.ParseSolanaAccountSettings(solanaSettings)
	require.NoError(t, err)
	require.Equal(t, "helius", parsed.RPCProvider)
	// or#881: the example declares NO tokens. The curated registry already
	// carries USDC (and decimals come from the chain, #817), so re-typing a
	// canonical mint here would be a money-path paste error, not documentation.
	require.Empty(t, parsed.Tokens)
	require.Equal(t, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		solanatokens.DefaultSupportedTokens()["USDC"].Mint)
}

func TestExampleAuthKitAuthorityManifestParses(t *testing.T) {
	_, err := akembedded.LoadBootstrapManifestFile(filepath.Join("..", "..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)
}

func TestManifestSolanaSignerEvidence(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	accountID := solanago.PublicKeyFromBytes(pub).String()
	localKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	localAccountID := localKey.PublicKey().String()
	secrets, err := newManifestSecretValues("solana", map[string]string{
		"private_key": localKey.String(),
	})
	require.NoError(t, err)

	got, gotAccountID, err := manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{}, secrets, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"mode": "local_keypair"}, got)
	require.Equal(t, localAccountID, gotAccountID)

	// A declared account_id is IGNORED (warned), never an error — derived from the key.
	_, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", accountID, ProviderRailAccountConfig{
		Signer: &PSPSignerConfig{Mode: "local_keypair"},
	}, secrets, nil)
	require.NoError(t, err)
	require.Equal(t, localAccountID, gotAccountID, "declared account_id ignored; derived from the keypair")

	_, _, err = manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{
		Signer: &PSPSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
	}, secrets, fakeTransit{pub: pub})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot also set secrets.private_key")

	emptySecrets, err := newManifestSecretValues("solana", nil)
	require.NoError(t, err)
	// vault_transit with a declared account_id: ignored (warned); derives from the Transit key.
	_, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", accountID, ProviderRailAccountConfig{
		Signer: &PSPSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
	}, emptySecrets, fakeTransit{pub: pub})
	require.NoError(t, err)
	require.Equal(t, accountID, gotAccountID, "declared account_id ignored; derived from the Transit key")

	got, gotAccountID, err = manifestProviderSignerEvidence(context.Background(), "solana", "", ProviderRailAccountConfig{
		Signer: &PSPSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"},
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
		return "version: 1\nmerchants:\n  host-three:\n    display_name: Host Three\n" + fragment
	}
	for _, row := range []struct{ name, body, want string }{
		{"unknown top-level key", "version: 1\ntenantz: []\n", "tenantz"},
		{"auth section removed (hard cut)", "version: 1\nauth:\n  users: []\nmerchants:\n  x:\n    display_name: X\n", "auth"},
		{"authkit authority belongs elsewhere", "users:\n  - username: operator\n", "users"},
		{"no merchants", "version: 1\n", "at least one merchant"},
		{"missing merchant display name", "version: 1\nmerchants:\n  host-three: {}\n", `merchant "host-three" display_name is required`},
		{"merchant name removed", "version: 1\nmerchants:\n  host-three:\n    name: Host Three\n", "unknown field \"name\""},
		{"support email removed", base("    profile:\n      support_email: support@example.com\n"), "support_email"},
		{"issuer section removed", base("    issuer:\n      issuer: https://auth.host-three.example\n      jwks_uri: https://auth.host-three.example/.well-known/jwks.json\n"), "issuer"},
		{"remote application missing issuer", base("    remote_application:\n      jwks_uri: https://auth.host-three.example/.well-known/jwks.json\n"), "remote_application.issuer is required"},
		{"remote application both trust sources", base("    remote_application:\n      issuer: https://auth.host-three.example\n      jwks_uri: https://auth.host-three.example/jwks\n      public_keys:\n        - public_key_pem: x\n"), "exactly one of jwks_uri, jwks, or public_keys"},
		{"remote application no trust source", base("    remote_application:\n      issuer: https://auth.host-three.example\n"), "must set jwks_uri, jwks, or public_keys"},
		{"remote application allowed origins removed", base("    remote_application:\n      issuer: https://auth.host-three.example\n      jwks_uri: https://auth.host-three.example/jwks\n      allowed_origins:\n        - https://auth.host-three.example\n"), "allowed_origins"},
		{"catalogs belong to push-merchant-catalog", "version: 1\ncatalogs: []\n", "catalogs"},
		{"invalid profile URL", base("    profile:\n      logo_url: ftp://cdn.example/logo.png\n"), "profile.logo_url"},
		{"api_host with scheme rejected (#850)", base("    api_host: https://api.host-three.example\n"), "api_host"},
		{"api_host with path rejected (#850)", base("    api_host: api.host-three.example/v1\n"), "api_host"},
		{"renamed key rail_merchant_accounts rejected with pointer (#698)", base("    rail_merchant_accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n"), "merchants.host-three.rail_merchant_accounts was renamed to psps"},
		{"pre-#683 key provider_accounts rejected with pointer", base("    provider_accounts:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n"), "merchants.host-three.provider_accounts was renamed to psps"},
		{"PSP routing removed", base("    psps:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          routing: standby\n"), "unknown field \"routing\""},
		{"PSP mode removed", base("    psps:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          mode: primary\n"), "unknown field \"mode\""},
		{"PSP role removed", base("    psps:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          role: primary\n"), "unknown field \"role\""},
		{"declared psp environment is retired", base("    psps:\n      stripe:\n        stripe:\n          environment: live\n          account_id: acct_test_123\n"), "psps.stripe.stripe.environment was removed (#882)"},
		{"solana network is not a PSP knob", base("    psps:\n      solana:\n        solana:\n          network: devnet\n"), "unknown field \"network\""},
		{"invalid provider secret alias", base("    psps:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n          secrets:\n            api_key: one\n"), "unknown PSP secret"},
		{"nmi tokenization key is a setting", base("    psps:\n      mobius:\n        nmi:\n          account_id: mobius-profile-id\n          secrets:\n            tokenization_key: public-token\n"), "unknown PSP secret"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(row.body))
			require.ErrorContains(t, err, row.want)
		})
	}
}

// A declared Solana account_id is IGNORED, not rejected: parsing succeeds (it is
// derived from the signer at apply, with a warning). A Solana account with no
// signer to derive from is the only Solana parse error.
func TestParseMerchantConfigManifestSolanaAccountIDIgnored(t *testing.T) {
	base := func(fragment string) string {
		return "version: 1\nmerchants:\n  host-three:\n    display_name: Host Three\n" + fragment
	}
	withAccountID := base("    psps:\n      solana:\n        solana:\n          account_id: AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9\n          signer: { mode: local_keypair }\n          secrets:\n            private_key: 2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT\n")
	_, err := ParseMerchantConfigManifest([]byte(withAccountID))
	require.NoError(t, err, "declared solana account_id is ignored, not a parse error")

	noSigner := base("    psps:\n      solana:\n        solana:\n          archived: false\n")
	_, err = ParseMerchantConfigManifest([]byte(noSigner))
	require.ErrorContains(t, err, "requires a signer", "solana with no signer has no key to derive account_id from")
}
