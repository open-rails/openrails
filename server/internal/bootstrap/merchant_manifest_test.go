package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestExampleManifestsParse(t *testing.T) {
	manifest, err := LoadMerchantConfigManifest(filepath.Join("..", "..", "..", "config", "merchants_config.example.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.Merchants, 2)
	require.Equal(t, "https://local-stack.example/.well-known/jwks.json", manifest.Merchants["local-stack"].RemoteApplication.JWKSURI)
	require.Len(t, manifest.Merchants["static-jwks-stack"].RemoteApplication.JWKS.Keys, 1)

	m := manifest.Merchants["local-stack"]
	require.NotNil(t, m.MerchantSettings.UsageBilling)
	require.Equal(t, 13, m.MerchantSettings.UsageBilling.BillingDay)
	require.Equal(t, []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 100_000_000, MinimumInvoiceAmount: 1_000_000}}, m.MerchantSettings.UsageBilling.Currencies)
	require.Equal(t, 7, *m.MerchantSettings.ArrearsGraceDays)
	require.Equal(t, []billing.BudgetWindow{{Key: "burst", WindowSeconds: 900, Limit: 5_000_000}, {Key: "sustained", WindowSeconds: 18_000, Limit: 20_000_000}}, m.MerchantSettings.DelegatedInvokerWastedSpendLimits)
	require.Len(t, *m.MerchantSettings.PSPRouting, 3)
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
	require.Equal(t, config.ClassTest, bt.Environment, "the custodian's money class matches its test PSPs")
	require.NoError(t, config.ValidateCustodianEntry(config.CustodianEntry{
		Key: "bt", Kind: models.CustodianBasisTheory, AccountID: bt.AccountID, Settings: bt.Settings, SecretKeys: []string{custodians.SecretAPIKey},
	}))
	require.NotEmpty(t, bt.Secrets[custodians.SecretAPIKey])

	require.Equal(t, "devnet", byName["solana"].Environment)
	require.Equal(t, "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu", byName["solana"].AccountID, "a Solana PSP's account is its treasury")
	require.NotEmpty(t, byName["mobius"].Settings["tokenization_key"])
}

func TestMerchantManifestValidation(t *testing.T) {
	base := func(fragment string) string {
		return "version: 1\nmerchants:\n  host-three:\n    display_name: Host Three\n" + fragment
	}
	psp := func(fragment string) string {
		return base("    psps:\n      stripe:\n        rail: stripe\n        environment: test\n        account_id: acct_test_123\n" + fragment)
	}
	remote := func(fragment string) string {
		return base("    remote_application:\n" + fragment)
	}
	treasury := solanago.NewWallet().PublicKey().String()
	for name, tc := range map[string]struct{ body, want string }{
		"unknown top-level key":          {"version: 1\ntenantz: []\n", "tenantz"},
		"auth section":                   {"version: 1\nauth:\n  users: []\nmerchants:\n  x:\n    display_name: X\n", "auth"},
		"authkit authority":              {"users:\n  - username: operator\n", "users"},
		"catalogs":                       {"version: 1\ncatalogs: []\n", "catalogs"},
		"no merchants":                   {"version: 1\n", "at least one merchant"},
		"wrong version":                  {"version: 2\nmerchants:\n  x:\n    display_name: X\n", "version must be 1"},
		"merchant name removed":          {"version: 1\nmerchants:\n  host-three:\n    name: Host Three\n", `unknown field "name"`},
		"support email removed":          {base("    profile:\n      support_email: s@example.com\n"), "support_email"},
		"settings block removed":         {base("    settings:\n      monthly_floor: 1\n"), `unknown field "settings"`},
		"invoice block retired":          {base("    invoice:\n      monthly_floor: 1\n"), `unknown field "invoice"`},
		"window as duration":             {base("    delegated_invoker_wasted_spend_limits:\n      - { key: burst, window: 15m, limit: 5 }\n"), `unknown field "window"`},
		"negative floor":                 {base("    monthly_floor: -1\n"), `merchant "host-three" settings: monthly_floor`},
		"policy names an undeclared":     {base("    billing_policy_bindings:\n      - { policy: nope }\n"), "undeclared policy"},
		"issuer section removed":         {base("    issuer:\n      issuer: https://auth.example\n"), "issuer"},
		"profile URL scheme":             {base("    profile:\n      logo_url: ftp://cdn.example/logo.png\n"), "profile.logo_url"},
		"api_host removed":               {base("    api_host: api.host-three.example\n"), "merchants.host-three.api_host was removed"},
		"remote without issuer":          {remote("      jwks_uri: https://auth.example/jwks\n"), "remote_application.issuer is required"},
		"remote two trust sources":       {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      public_keys:\n        - public_key_pem: x\n"), "exactly one of jwks_uri, jwks, or public_keys"},
		"remote no trust source":         {remote("      issuer: https://auth.example\n"), "must set jwks_uri, jwks, or public_keys"},
		"remote non-http jwks":           {remote("      issuer: https://auth.example\n      jwks_uri: file:///etc/jwks\n"), "jwks_uri must be an http"},
		"remote allowed origins":         {remote("      issuer: https://auth.example\n      jwks_uri: https://auth.example/jwks\n      allowed_origins: [https://auth.example]\n"), "allowed_origins"},
		"renamed rail accounts":          {base("    rail_merchant_accounts: {}\n"), "merchants.host-three.rail_merchant_accounts was renamed to psps"},
		"renamed provider accounts":      {base("    provider_accounts: {}\n"), "merchants.host-three.provider_accounts was renamed to psps"},
		"slug inside an entry":           {base("    slug: host-three\n"), "merchants.host-three.slug is not accepted: the entry's key is its slug"},
		"psp routing removed":            {psp("        routing: standby\n"), `psps.stripe: a stripe PSP has no "routing"`},
		"psp settings block removed":     {psp("        settings: {publishable_key: pk_test_1}\n"), `psps.stripe: a stripe PSP has no "settings"`},
		"psp environment the rail lacks": {base("    psps:\n      stripe:\n        rail: stripe\n        environment: sandbox\n        account_id: acct_test_123\n"), `merchant "host-three" psps.stripe.environment: stripe has no "sandbox" environment (live, test)`},
		"unknown secret":                 {psp("        secrets: {api_key: one}\n"), `merchant "host-three" psps.stripe.secrets: unknown field "api_key" (stripe takes secret_key, webhook_signing_secret, webhook_signing_secret_previous, webhook_signing_secret_thin)`},
		"stripe field typo":              {psp("        publishable_kye: pk_test_1\n"), `psps.stripe: a stripe PSP has no "publishable_kye" (stripe takes rail, environment, archived, account_id, publishable_key, webhook_overlap_expires_at, secrets)`},
		"nmi field typo":                 {base("    psps:\n      mobius:\n        rail: nmi\n        environment: sandbox\n        gateway_id: p\n        tokenization_kye: t\n"), `psps.mobius: a nmi PSP has no "tokenization_kye" (nmi takes rail, environment, archived, gateway_id, custodian, tokenization_key, tokenization_url, card_entry, webhook_overlap_expires_at, secrets)`},
		"nmi secret typo":                {base("    psps:\n      mobius:\n        rail: nmi\n        environment: sandbox\n        gateway_id: p\n        secrets: {security_kye: s}\n"), `merchant "host-three" psps.mobius.secrets: unknown field "security_kye"`},
		"ccbill has only its account":    {base("    psps:\n      ccbill:\n        rail: ccbill\n        environment: sandbox\n        client_accnum: \"999999\"\n        client_subacc: \"0000\"\n        salt: s\n"), `psps.ccbill: a ccbill PSP has no "salt" (ccbill takes rail, environment, archived, client_accnum, client_subacc, secrets)`},
		"ccbill secret typo":             {base("    psps:\n      ccbill:\n        rail: ccbill\n        environment: sandbox\n        client_accnum: \"999999\"\n        client_subacc: \"0000\"\n        secrets: {slat: s}\n"), `merchant "host-three" psps.ccbill.secrets: unknown field "slat" (ccbill takes datalink_password, datalink_username, salt)`},
		"solana field typo":              {base("    psps:\n      solana:\n        rail: solana\n        environment: devnet\n        treasury_walet: w\n"), `psps.solana: a solana PSP has no "treasury_walet" (solana takes rail, environment, archived, treasury, secrets)`},
		"solana signer removed":          {base("    psps:\n      solana:\n        rail: solana\n        treasury: " + treasury + "\n        signer: { mode: local_keypair }\n"), `a solana PSP has no "signer"`},
		"solana account_id":              {base("    psps:\n      solana:\n        rail: solana\n        account_id: " + treasury + "\n"), `a solana PSP has no "account_id"`},
		"solana secret typo":             {base("    psps:\n      solana:\n        rail: solana\n        treasury: " + treasury + "\n        secrets: {private_key: k}\n"), `merchant "host-three" psps.solana.secrets: unknown field "private_key" (solana takes rpc_url)`},
		"solana sandbox":                 {base("    psps:\n      solana:\n        rail: solana\n        environment: sandbox\n        treasury: " + treasury + "\n"), `merchant "host-three" psps.solana.environment: solana has no "sandbox" environment (mainnet, devnet, testnet)`},
		"solana without treasury":        {base("    psps:\n      solana:\n        rail: solana\n        environment: devnet\n"), `merchant "host-three" psps.solana.treasury is required`},
		"solana treasury not an address": {base("    psps:\n      solana:\n        rail: solana\n        treasury: nope\n"), `treasury "nope" is not a Solana address`},
		"solana http rpc_url":            {base("    psps:\n      solana:\n        rail: solana\n        treasury: " + treasury + "\n        secrets: {rpc_url: \"http://rpc.example/key-123\"}\n"), "secrets.rpc_url must be https"},
		"unknown rail":                   {base("    psps:\n      mobius:\n        rail: nmii\n        gateway_id: p\n"), `psps.mobius: unknown rail "nmii" (ccbill, nmi, solana, stripe)`},
		"psp without rail":               {base("    psps:\n      mobius:\n        gateway_id: p\n"), "psps.mobius: rail is required"},
		"psp rail-keyed block":           {base("    psps:\n      mobius:\n        nmi:\n          gateway_id: p\n"), "psps.mobius: rail is required"},
		"nmi tokenization is a field":    {base("    psps:\n      mobius:\n        rail: nmi\n        environment: sandbox\n        gateway_id: p\n        secrets: {tokenization_key: t}\n"), `psps.mobius.secrets: unknown field "tokenization_key"`},
		"solana network not a field":     {base("    psps:\n      solana:\n        rail: solana\n        network: devnet\n"), `a solana PSP has no "network"`},
		"custodian settings block":       {base("    custodians:\n      bt:\n        kind: basis_theory\n        account_id: t\n        settings: {public_api_key: k}\n"), `custodians.bt: a basis_theory custodian has no "settings"`},
		"nmi account_id":                 {base("    psps:\n      mobius:\n        rail: nmi\n        account_id: \"1233466\"\n"), `psps.mobius: a nmi PSP has no "account_id" (nmi takes rail, environment, archived, gateway_id,`},
		"nmi without gateway_id":         {base("    psps:\n      mobius:\n        rail: nmi\n        environment: sandbox\n"), `merchant "host-three" psps.mobius.gateway_id is required`},
		"nmi unquoted gateway_id":        {base("    psps:\n      mobius:\n        rail: nmi\n        gateway_id: 1233466\n"), `psps.mobius: gateway_id must be a string: quote it (gateway_id: "1233466")`},
		"ccbill account_id":              {base("    psps:\n      ccbill:\n        rail: ccbill\n        account_id: \"999999-0000\"\n"), `psps.ccbill: a ccbill PSP has no "account_id" (ccbill takes rail, environment, archived, client_accnum, client_subacc, secrets)`},
		"ccbill without subaccount":      {base("    psps:\n      ccbill:\n        rail: ccbill\n        client_accnum: \"999999\"\n"), `merchant "host-three" psps.ccbill.client_subacc is required`},
		"ccbill subaccount not a number": {base("    psps:\n      ccbill:\n        rail: ccbill\n        client_accnum: \"999999\"\n        client_subacc: \"00/0\"\n"), `merchant "host-three" psps.ccbill: client_subacc must be CCBill's number, digits only (got "00/0")`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMerchantConfigManifest([]byte(tc.body))
			require.ErrorContains(t, err, tc.want)
		})
	}

	// Omitted environments are the rails' real-money ones.
	m, err := ParseMerchantConfigManifest([]byte(base("    psps:\n      stripe:\n        rail: stripe\n        account_id: acct_123\n      solana:\n        rail: solana\n        treasury: " + treasury + "\n        secrets: {rpc_url: \"https://mainnet.helius-rpc.com/?api-key=k\"}\n")))
	require.NoError(t, err)
	require.Equal(t, "live", m.Merchants["host-three"].PSPs["stripe"].Environment)
	require.Equal(t, "mainnet", m.Merchants["host-three"].PSPs["solana"].Environment)
	require.Equal(t, treasury, m.Merchants["host-three"].PSPs["solana"].AccountID)
	m, err = ParseMerchantConfigManifest([]byte(base("    psps:\n      ccbill:\n        rail: ccbill\n        environment: sandbox\n        client_accnum: \"999999\"\n        client_subacc: \"0000\"\n")))
	require.NoError(t, err)
	require.Equal(t, "999999-0000", m.Merchants["host-three"].PSPs["ccbill"].AccountID, "the account CCBill's two numbers name")
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
