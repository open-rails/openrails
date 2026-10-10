package bootstrap

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	solanatransit "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
)

func TestExampleManifestsParse(t *testing.T) {
	manifest, err := LoadMerchantConfigManifest(filepath.Join("..", "..", "..", "config", "merchants_config.example.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 2)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", manifest.Merchants["local-stack"].RemoteApplication.JWKSURI)
	require.Len(t, manifest.Merchants["static-jwks-stack"].RemoteApplication.JWKS.Keys, 1)

	m := manifest.Merchants["local-stack"]
	require.Equal(t, "calendar_month", m.Settings.InvoiceBillingBoundary)
	require.Equal(t, int64(50_000_000), *m.Settings.InvoiceCollectionThreshold)
	require.Equal(t, int64(1_000_000), *m.Settings.InvoiceMonthlyFloor)
	require.Equal(t, 7, *m.Settings.ArrearsGraceDays)
	require.Equal(t, []billing.BudgetWindow{{Key: "burst", WindowSeconds: 900, Limit: 5_000_000}, {Key: "sustained", WindowSeconds: 18_000, Limit: 20_000_000}}, m.Settings.DelegatedInvokerWastedSpendLimits)
	require.Len(t, *m.Settings.CheckoutRouting, 3)
	require.Equal(t, "Local Stack Billing", m.DisplayName)

	byName, railOf := m.PSPs, map[string]string{}
	for name, account := range m.PSPs {
		railOf[name] = string(account.Rail)
	}
	require.Equal(t, "nmi", railOf["mobius"])
	require.Equal(t, "nmi", railOf["mobius-bt"])
	require.Equal(t, "bt", byName["mobius-bt"].Custodian)
	require.Equal(t, "bt", byName["mobius-bt-backup"].Custodian)
	require.Empty(t, byName["mobius-bt"].Settings, "custody is a reference, not PSP settings")
	require.True(t, byName["paykings"].Archived)
	require.Equal(t, "999999-0000", byName["ccbill"].AccountID)

	bt := m.Custodians["bt"]
	require.Equal(t, models.CustodianBasisTheory, bt.Kind)
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
		return base("    psps:\n      stripe:\n        rail: stripe\n        account_id: acct_test_123\n" + fragment)
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
		"merchant name removed":        {"version: 1\nmerchants:\n  host-three:\n    name: Host Three\n", `unknown field "name"`},
		"support email removed":        {base("    settings:\n      profile:\n        support_email: s@example.com\n"), "support_email"},
		"settings outside settings":    {base("    profile:\n      display_name: X\n"), `unknown field "profile"`},
		"invoice block retired":        {base("    invoice:\n      monthly_floor: 1\n"), `unknown field "invoice"`},
		"window as duration":           {base("    settings:\n      delegated_invoker_wasted_spend_limits:\n        - { key: burst, window: 15m, limit: 5 }\n"), `unknown field "window"`},
		"negative floor":               {base("    settings:\n      monthly_floor: -1\n"), "settings"},
		"policy names an undeclared":   {base("    settings:\n      billing_policy_bindings:\n        - { policy: nope }\n"), "undeclared policy"},
		"issuer section removed":       {base("    issuer:\n      issuer: https://auth.example\n"), "issuer"},
		"profile URL scheme":           {base("    settings:\n      profile:\n        logo_url: ftp://cdn.example/logo.png\n"), "profile.logo_url"},
		"api_host with scheme":         {base("    api_host: https://api.host-three.example\n"), "api_host"},
		"api_host with path":           {base("    api_host: api.host-three.example/v1\n"), "api_host"},
		"remote without issuer":        {remote("      jwks_uri: https://auth.example/jwks\n"), "remote_application.issuer is required"},
		"remote two trust sources":     {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      public_keys:\n        - public_key_pem: x\n"), "exactly one of jwks_uri, jwks, or public_keys"},
		"remote no trust source":       {remote("      issuer: https://auth.example\n"), "must set jwks_uri, jwks, or public_keys"},
		"remote non-http jwks":         {remote("      issuer: https://auth.example\n      jwks_uri: file:///etc/jwks\n"), "jwks_uri must be an http"},
		"remote allowed origins":       {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      allowed_origins: [https://auth.example]\n"), "allowed_origins"},
		"renamed rail accounts":        {base("    rail_merchant_accounts: {}\n"), "merchants.host-three.rail_merchant_accounts was renamed to psps"},
		"renamed provider accounts":    {base("    provider_accounts: {}\n"), "merchants.host-three.provider_accounts was renamed to psps"},
		"slug inside an entry":         {base("    slug: host-three\n"), "merchants.host-three.slug is not accepted: the entry's key is its slug"},
		"psp routing removed":          {psp("        routing: standby\n"), `unknown field "routing"`},
		"psp mode removed":             {psp("        mode: primary\n"), `unknown field "mode"`},
		"psp role removed":             {psp("        role: primary\n"), `unknown field "role"`},
		"psp environment retired":      {psp("        environment: live\n"), `unknown field "environment"`},
		"unknown secret":               {psp("        secrets: {api_key: one}\n"), `merchant "host-three" psps.stripe.secrets: unknown field "api_key" (stripe takes secret_key, webhook_signing_secret, webhook_signing_secret_previous, webhook_signing_secret_thin)`},
		"stripe setting typo":          {psp("        settings: {publishable_kye: pk_test_1}\n"), `merchant "host-three" psps.stripe.settings: unknown field "publishable_kye" (stripe takes publishable_key, webhook_overlap_expires_at)`},
		"nmi setting typo":             {base("    psps:\n      mobius:\n        rail: nmi\n        account_id: p\n        settings: {tokenization_kye: t}\n"), `merchant "host-three" psps.mobius.settings: unknown field "tokenization_kye" (nmi takes card_entry, endpoint_deployment, tokenization_key, tokenization_url, webhook_overlap_expires_at)`},
		"nmi secret typo":              {base("    psps:\n      mobius:\n        rail: nmi\n        account_id: p\n        secrets: {security_kye: s}\n"), `merchant "host-three" psps.mobius.secrets: unknown field "security_kye"`},
		"ccbill takes no settings":     {base("    psps:\n      ccbill:\n        rail: ccbill\n        account_id: 999999-0000\n        settings: {salt: s}\n"), `merchant "host-three" psps.ccbill.settings: unknown field "salt" (ccbill takes no settings)`},
		"ccbill secret typo":           {base("    psps:\n      ccbill:\n        rail: ccbill\n        account_id: 999999-0000\n        secrets: {slat: s}\n"), `merchant "host-three" psps.ccbill.secrets: unknown field "slat" (ccbill takes datalink_password, datalink_username, salt)`},
		"solana setting typo":          {base("    psps:\n      solana:\n        rail: solana\n        signer: { mode: local_keypair }\n        settings: {recipient_walet: w}\n"), `merchant "host-three" psps.solana.settings: unknown field "recipient_walet" (solana takes recipient_wallet, rpc_api_key, rpc_provider, tokens)`},
		"solana secret typo":           {base("    psps:\n      solana:\n        rail: solana\n        signer: { mode: local_keypair }\n        secrets: {private_kye: k}\n"), `merchant "host-three" psps.solana.secrets: unknown field "private_kye" (solana takes private_key)`},
		"unknown rail":                 {base("    psps:\n      mobius:\n        rail: nmii\n        account_id: p\n"), `merchant "host-three" psps.mobius.rail: unknown rail "nmii"`},
		"psp without rail":             {base("    psps:\n      mobius:\n        account_id: p\n"), "psps.mobius.rail is required"},
		"psp rail-keyed block":         {base("    psps:\n      mobius:\n        nmi:\n          account_id: p\n"), `unknown field "nmi"`},
		"nmi tokenization is setting":  {base("    psps:\n      mobius:\n        rail: nmi\n        account_id: p\n        secrets: {tokenization_key: t}\n"), `psps.mobius.secrets: unknown field "tokenization_key"`},
		"solana network not a PSP key": {base("    psps:\n      solana:\n        rail: solana\n        network: devnet\n"), `unknown field "network"`},
		"solana without signer":        {base("    psps:\n      solana:\n        rail: solana\n        archived: false\n"), "requires a signer"},
		"ccbill slash account":         {base("    psps:\n      ccbill:\n        rail: ccbill\n        account_id: \"999999/0000\"\n"), "CCBill account_id uses a dash"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(tc.body))
			require.ErrorContains(t, err, tc.want)
		})
	}

	// A declared Solana account_id is ignored (derived from the signer), not an error.
	_, err := ParseMerchantConfigManifest([]byte(base("    psps:\n      solana:\n        rail: solana\n        account_id: AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9\n        signer: { mode: local_keypair }\n        secrets:\n          private_key: 2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT\n")))
	require.NoError(t, err)
	m, err := ParseMerchantConfigManifest([]byte(base("    psps:\n      ccbill:\n        rail: ccbill\n        account_id: \"999999-0000\"\n")))
	require.NoError(t, err)
	require.Equal(t, "999999-0000", m.Merchants["host-three"].PSPs["ccbill"].AccountID)
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
	cfg := &config.Config{}
	for _, seed := range []bool{false, true} {
		for _, insert := range []bool{false, true} {
			opts, err := ResolvePushMerchantConfigOptions(cfg, seed, insert, false, false)
			require.NoError(t, err)
			require.Equal(t, seed || insert, opts.Insert)
		}
		for _, flags := range [][2]bool{{true, false}, {false, true}, {true, true}} {
			_, err := ResolvePushMerchantConfigOptions(cfg, seed, true, flags[0], flags[1])
			require.ErrorContains(t, err, "at its revision")
		}
	}
}

type fakeTransit struct{ pub []byte }

func (fakeTransit) Sign(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (f fakeTransit) PublicKey(context.Context, string) ([]byte, error)  { return f.pub, nil }

// A Solana account_id is always derived from the signer; a declared one is
// ignored, and a Transit signer cannot also carry a local private key.
func TestSolanaSignerAccount(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	transitAccount := solanago.PublicKeyFromBytes(pub).String()
	local, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	withKey := map[string]string{"private_key": local.String()}
	transit := &PSPSignerConfig{Mode: "vault_transit", Key: "openrails-solana-local"}
	ctx := context.Background()

	for name, tc := range map[string]struct {
		account     PSPConfig
		wantAccount string
	}{
		"implicit local keypair":        {PSPConfig{Secrets: withKey}, local.PublicKey().String()},
		"declared id ignored (local)":   {PSPConfig{AccountID: transitAccount, Signer: &PSPSignerConfig{Mode: "local_keypair"}, Secrets: withKey}, local.PublicKey().String()},
		"transit":                       {PSPConfig{Signer: transit}, transitAccount},
		"declared id ignored (transit)": {PSPConfig{AccountID: "not-the-key", Signer: transit}, transitAccount},
	} {
		account, err := merchantbootstrap.SolanaAccountID(ctx, tc.account, fakeTransit{pub: pub})
		require.NoError(t, err, name)
		require.Equal(t, tc.wantAccount, account, name)
	}
	for want, tc := range map[string]struct {
		account PSPConfig
		transit bool
	}{
		"cannot also set secrets.private_key":    {PSPConfig{Signer: transit, Secrets: withKey}, true},
		"requires a Vault connection":            {PSPConfig{Signer: transit}, false},
		"local_keypair requires secrets.private": {PSPConfig{Signer: &PSPSignerConfig{Mode: "local_keypair"}}, true},
		"must be local_keypair or vault_transit": {PSPConfig{Signer: &PSPSignerConfig{Mode: "hsm"}}, true},
	} {
		var client solanatransit.TransitClient
		if tc.transit {
			client = fakeTransit{pub: pub}
		}
		_, err := merchantbootstrap.SolanaAccountID(ctx, tc.account, client)
		require.ErrorContains(t, err, want)
	}
}
