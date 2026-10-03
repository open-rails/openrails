package config

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// Every retired inline-custody key fails loudly and names itself: accepted but
// inert custody on a money path reads as "this works".
func TestRetiredCustodySettingsRefused(t *testing.T) {
	for _, key := range []string{
		"custodian", "custodian_account_id", "custodian_public_api_key", "custodian_network_tokens",
		"custodian_api_key", "gateway_account", "nt_charges", "  Custodian_Account_ID ",
	} {
		require.ErrorContains(t, RejectRetiredCustodySettings(map[string]any{key: "x"}), key)
	}
	require.NoError(t, RejectRetiredCustodySettings(map[string]any{"tokenization_key": "k", "rpc_provider": "helius"}))
	require.NoError(t, RejectRetiredCustodySettings(nil))
}

func TestValidateCustodianEntry(t *testing.T) {
	valid := func() CustodianEntry {
		return CustodianEntry{
			Key: "bt", Kind: models.CustodianBasisTheory, AccountID: "tnt_test",
			Settings:   map[string]any{custodians.SettingPublicAPIKey: "key_pub", custodians.SettingNetworkTokens: false},
			SecretKeys: []string{custodians.SecretAPIKey},
		}
	}
	require.NoError(t, ValidateCustodianEntry(valid()))
	storePlane := valid()
	storePlane.SecretKeys = nil
	require.NoError(t, ValidateCustodianEntry(storePlane), "the store plane checks secrets at arm time")
	rotated := valid()
	rotated.CredentialVersions = map[string]int{custodians.SecretAPIKey: 3}
	require.NoError(t, ValidateCustodianEntry(rotated))

	for name, edit := range map[string]func(*CustodianEntry){
		"no key":               func(e *CustodianEntry) { e.Key = " " },
		"custody-less kind":    func(e *CustodianEntry) { e.Kind = models.CustodianPSP },
		"no account id":        func(e *CustodianEntry) { e.AccountID = "" },
		"no public key":        func(e *CustodianEntry) { delete(e.Settings, custodians.SettingPublicAPIKey) },
		"unknown setting":      func(e *CustodianEntry) { e.Settings["invented"] = 1 },
		"tenant endpoint":      func(e *CustodianEntry) { e.Settings["api_base_url"] = "https://tenant.example.test" },
		"wrong value type":     func(e *CustodianEntry) { e.Settings[custodians.SettingNetworkTokens] = "yes please" },
		"unknown secret":       func(e *CustodianEntry) { e.SecretKeys = []string{"security_key"} },
		"missing api key":      func(e *CustodianEntry) { e.SecretKeys = []string{} },
		"version unknown slot": func(e *CustodianEntry) { e.CredentialVersions = map[string]int{"security_key": 1} },
		"version negative":     func(e *CustodianEntry) { e.CredentialVersions = map[string]int{custodians.SecretAPIKey: -1} },
		"version not canonical": func(e *CustodianEntry) {
			e.CredentialVersions = map[string]int{"API_KEY": 1}
		},
	} {
		e := valid()
		edit(&e)
		require.Error(t, ValidateCustodianEntry(e), name)
	}

	// HyperSwitch endpoints belong to the host (validateHyperSwitch); no
	// merchant setting may carry one.
	hs := map[string]any{custodians.SettingPublicAPIKey: "pk", custodians.SettingProfileID: "profile"}
	_, err := custodians.ParseSettings(models.CustodianHyperSwitch, hs)
	require.NoError(t, err)
	for _, key := range []string{"api_base_url", "sdk_url", "gateway_url"} {
		hs[key] = "https://tenant-chosen.example.test"
		_, err = custodians.ParseSettings(models.CustodianHyperSwitch, hs)
		require.Error(t, err, key)
		delete(hs, key)
	}
}

func TestNMIEndpointDeployment(t *testing.T) {
	for settings, want := range map[string]string{"": "", NMIEndpointGateway: NMIEndpointGateway, NMIEndpointSandbox: NMIEndpointSandbox} {
		in := map[string]any{}
		if settings != "" {
			in["endpoint_deployment"] = settings
		}
		got, err := NMIEndpointDeployment(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, bad := range []any{"live", "", true} {
		_, err := NMIEndpointDeployment(map[string]any{"endpoint_deployment": bad})
		require.Error(t, err, "deployment is never credential posture: %v", bad)
	}
}

func TestSolanaAccountSettings(t *testing.T) {
	const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	s, err := ParseSolanaAccountSettings(map[string]any{
		"rpc_provider":     " Helius ",
		"rpc_api_key":      "key-123",
		"tokens":           map[string]any{"usdc": map[string]any{"mint": usdc, "name": "USD Coin"}, "sol": map[any]any{}},
		"recipient_wallet": "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu",
	})
	require.NoError(t, err)
	require.Equal(t, "helius", s.RPCProvider)
	require.Equal(t, map[string]TokenConfig{"USDC": {Mint: usdc, Name: "USD Coin"}, "SOL": {}}, s.Tokens,
		"a mint-less built-in symbol is valid here; the network-aware resolver rules on it")

	for want, settings := range map[string]map[string]any{
		// Decimals belong to the on-chain mint; a declared copy could misprice by 10^n.
		"read from the SPL mint on-chain":       {"tokens": map[string]any{"usdc": map[string]any{"mint": usdc, "decimals": 6.0}}},
		"unknown field":                         {"tokens": map[string]any{"USDC": map[string]any{"decimls": 6}}},
		"empty symbol":                          {"tokens": map[string]any{" ": map[string]any{}}},
		"must be a map":                         {"tokens": []any{"USDC"}},
		"rpc_provider must be helius or public": {"rpc_provider": "quicknode"},
		"public cannot use rpc_api_key":         {"rpc_provider": "public", "rpc_api_key": "k"},
		"must be a string":                      {"rpc_api_key": 42},
	} {
		_, err := ParseSolanaAccountSettings(settings)
		require.ErrorContains(t, err, want)
		require.ErrorContains(t, ValidateSolanaAccountSettings(settings), want)
	}

	typo := map[string]any{"rpc_provder": "helius"}
	s, err = ParseSolanaAccountSettings(typo)
	require.NoError(t, err, "stored rows stay forward compatible")
	require.True(t, s.IsZero())
	require.ErrorContains(t, ValidateSolanaAccountSettings(typo), "unknown key(s) rpc_provder")
	require.NoError(t, ValidateSolanaAccountSettings(map[string]any{"recipient_wallet": "w"}))

	base := &SolanaRailConfig{RPCProvider: "helius", RPCAPIKey: "boot-key", Tokens: map[string]TokenConfig{"SOL": {Mint: "So1"}}, Network: "devnet"}
	out := SolanaAccountSettings{RPCAPIKey: "store-key", Tokens: map[string]TokenConfig{"USDC": {}}}.ApplyTo(base)
	require.Equal(t, &SolanaRailConfig{RPCProvider: "helius", RPCAPIKey: "store-key", Tokens: map[string]TokenConfig{"SOL": {Mint: "So1"}}, Network: "devnet"}, out,
		"declared knobs win; tokens are resolved later by the network-aware layer")
	out.Tokens["X"] = TokenConfig{}
	require.Equal(t, "boot-key", base.RPCAPIKey)
	require.NotContains(t, base.Tokens, "X", "ApplyTo must never alias the boot plane")
	require.Equal(t, &SolanaRailConfig{RPCAPIKey: "store-key"}, SolanaAccountSettings{RPCAPIKey: "store-key"}.ApplyTo(nil))
}

func TestSecretFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)
	for name, value := range map[string]string{"DB_PASSWORD": "s3cret\n", "jwt-secret": "host-owned", "EMPTY_VALUE": " \n", "..data": "k8s"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "NESTED"), 0o700))
	got, err := SecretFiles()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"DB_PASSWORD": "s3cret"}, got)

	t.Setenv("VAULT_SECRETS_PATH", filepath.Join(dir, "absent"))
	got, err = SecretFiles()
	require.NoError(t, err, "mounting secrets is optional")
	require.Nil(t, got)

	t.Setenv("VAULT_SECRETS_PATH", "")
	require.Equal(t, DefaultSecretsPath, SecretsPath())
}

// Reserved characters in credentials must not be able to redirect the DSN host.
func TestConnectionStringEncodesCredentials(t *testing.T) {
	c := &DBConfig{Host: "db.internal", Port: "5432", Database: "billing", Username: "open rails", Password: "p@ss/w:rd?#"}
	u, err := url.Parse(c.GetConnectionString())
	require.NoError(t, err)
	require.Equal(t, "postgresql", u.Scheme)
	require.Equal(t, "db.internal:5432", u.Host)
	require.Equal(t, "/billing", u.Path)
	require.Equal(t, "open rails", u.User.Username())
	pw, _ := u.User.Password()
	require.Equal(t, "p@ss/w:rd?#", pw)
	require.Equal(t, "require", u.Query().Get("sslmode"), "TLS unless explicitly disabled")

	c.SSLMode = "disable"
	u, err = url.Parse(c.GetConnectionString())
	require.NoError(t, err)
	require.Equal(t, "disable", u.Query().Get("sslmode"))

	require.Equal(t, "postgres://x/y", (&DBConfig{URL: "postgres://x/y", Host: "ignored", Port: "1", Database: "d", Username: "u"}).GetConnectionString())
	require.Empty(t, (&DBConfig{Host: "h", Port: "1", Database: "d"}).GetConnectionString(), "incomplete parts never guess")
}
