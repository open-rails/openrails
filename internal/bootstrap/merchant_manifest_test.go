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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	solanatransit "github.com/open-rails/openrails/internal/integrations/solana"
)

func TestExampleManifestsParse(t *testing.T) {
	_, err := akembedded.LoadBootstrapManifestFile(filepath.Join("..", "..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)

	manifest, err := LoadMerchantConfigManifest(filepath.Join("..", "..", "config", "merchants_config.example.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 2)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", manifest.Merchants["local-stack"].RemoteApplication.JWKSURI)
	require.Len(t, manifest.Merchants["static-jwks-stack"].RemoteApplication.JWKS.Keys, 1)

	m := manifest.Merchants["local-stack"]
	require.Equal(t, "calendar_month", m.Invoice.BillingPeriodBoundary)
	require.Equal(t, int64(50_000_000), *m.Invoice.CollectionThreshold)
	require.Equal(t, int64(1_000_000), *m.Invoice.MonthlyFloor)
	require.Len(t, m.DelegatedInvokerWastedSpendWindows, 2)

	byName, railOf := map[string]ProviderRailAccountConfig{}, map[string]string{}
	require.Len(t, m.PSPs, 10)
	for name, rails := range m.PSPs {
		require.Len(t, rails, 1, "one rail per named PSP")
		for rail, account := range rails {
			require.Empty(t, account.LegacyEnvironment, "environment is derived from posture, never declared")
			byName[name], railOf[name] = account, rail
		}
	}
	require.Equal(t, "nmi", railOf["mobius"])
	require.Equal(t, "nmi", railOf["mobius-bt"])
	require.Equal(t, "bt", byName["mobius-bt"].Custodian)
	require.Equal(t, "bt", byName["mobius-bt-backup"].Custodian)
	require.Empty(t, byName["mobius-bt"].Settings, "custody is a reference, not PSP settings")
	require.True(t, byName["paykings"].Archived)
	require.Equal(t, "999999-0000", byName["ccbill"].AccountID)

	bt := m.Custodians["bt"][models.CustodianBasisTheory]
	require.NoError(t, config.ValidateCustodianEntry(config.CustodianEntry{
		Key: "bt", Kind: models.CustodianBasisTheory, AccountID: bt.AccountID, Settings: bt.Settings, SecretKeys: []string{custodians.SecretAPIKey},
	}))
	require.NotEmpty(t, bt.Secrets[custodians.SecretAPIKey])

	solanaSettings, err := config.ParseSolanaAccountSettings(byName["solana"].Settings)
	require.NoError(t, err)
	require.Empty(t, solanaSettings.Tokens, "curated registry tokens are never re-declared")
}

func TestMerchantManifestValidation(t *testing.T) {
	base := func(fragment string) string {
		return "version: 1\nmerchants:\n  host-three:\n    display_name: Host Three\n" + fragment
	}
	psp := func(fragment string) string {
		return base("    psps:\n      stripe:\n        stripe:\n          account_id: acct_test_123\n" + fragment)
	}
	remote := func(fragment string) string {
		return base("    remote_application:\n" + fragment)
	}
	for name, tc := range map[string]struct{ body, want string }{
		"unknown top-level key":        {"version: 1\ntenantz: []\n", "tenantz"},
		"auth section":                 {"version: 1\nauth:\n  users: []\nmerchants:\n  x:\n    display_name: X\n", "auth"},
		"authkit authority":            {"users:\n  - username: operator\n", "users"},
		"catalogs":                     {"version: 1\ncatalogs: []\n", "catalogs"},
		"no merchants":                 {"version: 1\n", "at least one merchant"},
		"wrong version":                {"version: 2\nmerchants:\n  x:\n    display_name: X\n", "version must be 1"},
		"missing display name":         {"version: 1\nmerchants:\n  host-three: {}\n", `merchant "host-three" display_name is required`},
		"merchant name removed":        {"version: 1\nmerchants:\n  host-three:\n    name: Host Three\n", `unknown field "name"`},
		"support email removed":        {base("    profile:\n      support_email: s@example.com\n"), "support_email"},
		"issuer section removed":       {base("    issuer:\n      issuer: https://auth.example\n"), "issuer"},
		"profile URL scheme":           {base("    profile:\n      logo_url: ftp://cdn.example/logo.png\n"), "profile.logo_url"},
		"api_host with scheme":         {base("    api_host: https://api.host-three.example\n"), "api_host"},
		"api_host with path":           {base("    api_host: api.host-three.example/v1\n"), "api_host"},
		"remote without issuer":        {remote("      jwks_uri: https://auth.example/jwks\n"), "remote_application.issuer is required"},
		"remote two trust sources":     {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      public_keys:\n        - public_key_pem: x\n"), "exactly one of jwks_uri, jwks, or public_keys"},
		"remote no trust source":       {remote("      issuer: https://auth.example\n"), "must set jwks_uri, jwks, or public_keys"},
		"remote non-http jwks":         {remote("      issuer: https://auth.example\n      jwks_uri: file:///etc/jwks\n"), "jwks_uri must be an http"},
		"remote allowed origins":       {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      allowed_origins: [https://auth.example]\n"), "allowed_origins"},
		"renamed rail accounts":        {base("    rail_merchant_accounts: {}\n"), "merchants.host-three.rail_merchant_accounts was renamed to psps"},
		"renamed provider accounts":    {base("    provider_accounts: {}\n"), "merchants.host-three.provider_accounts was renamed to psps"},
		"psp routing removed":          {psp("          routing: standby\n"), `unknown field "routing"`},
		"psp mode removed":             {psp("          mode: primary\n"), `unknown field "mode"`},
		"psp role removed":             {psp("          role: primary\n"), `unknown field "role"`},
		"psp environment retired":      {psp("          environment: live\n"), "psps.stripe.stripe.environment was removed (#882)"},
		"unknown secret":               {psp("          secrets: {api_key: one}\n"), "unknown PSP secret"},
		"nmi tokenization is setting":  {base("    psps:\n      mobius:\n        nmi:\n          account_id: p\n          secrets: {tokenization_key: t}\n"), "unknown PSP secret"},
		"solana network not a PSP key": {base("    psps:\n      solana:\n        solana:\n          network: devnet\n"), `unknown field "network"`},
		"solana without signer":        {base("    psps:\n      solana:\n        solana:\n          archived: false\n"), "requires a signer"},
		"ccbill slash account":         {base("    psps:\n      ccbill:\n        ccbill:\n          account_id: \"945280/0000\"\n"), "CCBill account_id uses a dash"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(tc.body))
			require.ErrorContains(t, err, tc.want)
		})
	}

	// A declared Solana account_id is ignored (derived from the signer), not an error.
	_, err := ParseMerchantConfigManifest([]byte(base("    psps:\n      solana:\n        solana:\n          account_id: AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9\n          signer: { mode: local_keypair }\n          secrets:\n            private_key: 2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT\n")))
	require.NoError(t, err)
	m, err := ParseMerchantConfigManifest([]byte(base("    psps:\n      ccbill:\n        ccbill:\n          account_id: \"945280-0000\"\n")))
	require.NoError(t, err)
	require.Equal(t, "945280-0000", m.Merchants["host-three"].PSPs["ccbill"]["ccbill"].AccountID)
}

// The CLI file path is as strict as the bytes path: koanf alone would drop a
// typo'd field silently.
func TestManifestFilePathIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	m, err := LoadMerchantConfigManifest(write("valid.yaml", "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n"))
	require.NoError(t, err)
	require.Equal(t, "Acme", m.Merchants["acme"].DisplayName)
	_, err = LoadMerchantConfigManifest(write("typo.yaml", "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n    dispaly_name: Whoops\n"))
	require.ErrorContains(t, err, "dispaly_name")
	_, err = LoadMerchantConfigManifest(filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
	_, err = LoadMerchantConfigManifestFiles(" ")
	require.ErrorContains(t, err, "no path given")
}

func TestPushMerchantConfigIsCreateOnly(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		cfg := &config.Config{SecretBackend: backend}
		for _, seed := range []bool{false, true} {
			for _, insert := range []bool{false, true} {
				opts, err := ResolvePushMerchantConfigOptions(cfg, seed, insert, false, false)
				require.NoError(t, err)
				require.Equal(t, seed || insert, opts.Insert)
				require.False(t, opts.Overwrite || opts.Prune)
			}
			for _, flags := range [][2]bool{{true, false}, {false, true}, {true, true}} {
				_, err := ResolvePushMerchantConfigOptions(cfg, seed, true, flags[0], flags[1])
				require.ErrorContains(t, err, "expected revision")
			}
		}
	}
}

type fakeTransit struct{ pub []byte }

func (fakeTransit) Sign(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (f fakeTransit) PublicKey(context.Context, string) ([]byte, error)  { return f.pub, nil }

// A Solana account_id is always derived from the signer; a declared one is
// ignored, and a Transit signer cannot also carry a local private key.
func TestSolanaSignerEvidence(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	transitAccount := solanago.PublicKeyFromBytes(pub).String()
	local, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	withKey, err := newManifestSecretValues("solana", map[string]string{"private_key": local.String()})
	require.NoError(t, err)
	empty, err := newManifestSecretValues("solana", nil)
	require.NoError(t, err)
	transit := &PSPSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"}
	ctx := context.Background()

	for name, tc := range map[string]struct {
		declared    string
		account     ProviderRailAccountConfig
		secrets     manifestSecretValues
		wantAccount string
		wantSigner  map[string]string
	}{
		"implicit local keypair":        {"", ProviderRailAccountConfig{}, withKey, local.PublicKey().String(), map[string]string{"mode": "local_keypair"}},
		"declared id ignored (local)":   {transitAccount, ProviderRailAccountConfig{Signer: &PSPSignerConfig{Mode: "local_keypair"}}, withKey, local.PublicKey().String(), map[string]string{"mode": "local_keypair"}},
		"transit":                       {"", ProviderRailAccountConfig{Signer: transit}, empty, transitAccount, map[string]string{"mode": "vault_transit", "key": "openrails-solana-local"}},
		"declared id ignored (transit)": {"not-the-key", ProviderRailAccountConfig{Signer: transit}, empty, transitAccount, nil},
		"receive only":                  {"", ProviderRailAccountConfig{}, empty, "", nil},
	} {
		signer, account, err := manifestProviderSignerEvidence(ctx, "solana", tc.declared, tc.account, tc.secrets, fakeTransit{pub: pub})
		require.NoError(t, err, name)
		require.Equal(t, tc.wantAccount, account, name)
		if tc.wantSigner != nil {
			require.Equal(t, tc.wantSigner, signer, name)
		}
	}
	for want, tc := range map[string]struct {
		rail    string
		signer  *PSPSignerConfig
		secrets manifestSecretValues
		transit bool
	}{
		"cannot also set secrets.private_key":    {"solana", transit, withKey, true},
		"requires a Vault connection":            {"solana", transit, empty, false},
		"local_keypair requires secrets.private": {"solana", &PSPSignerConfig{Mode: "local_keypair"}, empty, true},
		"must be local_keypair or vault_transit": {"solana", &PSPSignerConfig{Mode: "hsm"}, empty, true},
		"only supported for solana":              {"stripe", transit, empty, true},
	} {
		var client solanatransit.TransitClient
		if tc.transit {
			client = fakeTransit{pub: pub}
		}
		_, _, err := manifestProviderSignerEvidence(ctx, tc.rail, "", ProviderRailAccountConfig{Signer: tc.signer}, tc.secrets, client)
		require.ErrorContains(t, err, want)
	}
}
