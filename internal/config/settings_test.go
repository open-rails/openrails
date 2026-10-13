package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"

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

	for name, edit := range map[string]func(*CustodianEntry){
		"no key":            func(e *CustodianEntry) { e.Key = " " },
		"custody-less kind": func(e *CustodianEntry) { e.Kind = models.CustodianPSP },
		"no account id":     func(e *CustodianEntry) { e.AccountID = "" },
		"no public key":     func(e *CustodianEntry) { delete(e.Settings, custodians.SettingPublicAPIKey) },
		"unknown setting":   func(e *CustodianEntry) { e.Settings["invented"] = 1 },
		"tenant endpoint":   func(e *CustodianEntry) { e.Settings["api_base_url"] = "https://tenant.example.test" },
		"wrong value type":  func(e *CustodianEntry) { e.Settings[custodians.SettingNetworkTokens] = "yes please" },
		"unknown secret":    func(e *CustodianEntry) { e.SecretKeys = []string{"security_key"} },
		"missing api key":   func(e *CustodianEntry) { e.SecretKeys = []string{} },
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

// card_entry (#1129) defaults to browser; server is an explicit declaration,
// refused where the gateway has no server-side vault call or a custodian
// takes the card.
func TestCardEntry(t *testing.T) {
	for _, tc := range []struct {
		rail      string
		settings  map[string]any
		custodial bool
		want      string
	}{
		{"nmi", nil, false, CardEntryBrowser},
		{"nmi", map[string]any{"tokenization_key": "tk"}, false, CardEntryBrowser},
		{"nmi", map[string]any{"card_entry": "browser"}, true, CardEntryBrowser},
		{"nmi", map[string]any{"card_entry": "server"}, false, CardEntryServer},
		{"stripe", map[string]any{"card_entry": "browser"}, false, CardEntryBrowser},
	} {
		got, err := CardEntry(tc.rail, tc.settings, tc.custodial)
		require.NoError(t, err, "%s %v", tc.rail, tc.settings)
		require.Equal(t, tc.want, got, "%s %v", tc.rail, tc.settings)
	}
	for name, tc := range map[string]struct {
		rail      string
		value     any
		custodial bool
	}{
		"stripe has no server vault call": {"stripe", "server", false},
		"ccbill has no server vault call": {"ccbill", "server", false},
		"solana has no card":              {"solana", "server", false},
		"unknown rail":                    {"unknown", "server", false},
		"a custodian takes the card":      {"nmi", "server", true},
		"not a mode":                      {"nmi", "SERVER", false},
		"empty":                           {"nmi", "", false},
		"not a string":                    {"nmi", true, false},
	} {
		_, err := CardEntry(tc.rail, map[string]any{"card_entry": tc.value}, tc.custodial)
		require.Error(t, err, name)
	}
}

// A Solana PSP is its treasury, a Solana address; its rpc_url is https (http
// only on a loopback host of a non-live PSP) and no message repeats it. The
// cranker is one deployment key.
func TestSolanaPSPAndCranker(t *testing.T) {
	treasury := solanago.NewWallet().PublicKey().String()
	require.NoError(t, ValidateSolanaTreasury(treasury))
	require.ErrorContains(t, ValidateSolanaTreasury(""), "treasury is required")
	require.ErrorContains(t, ValidateSolanaTreasury("not-an-address"), "is not a Solana address")
	require.ErrorContains(t, ValidatePSPAccount("wallet", "solana", ""), "psps.wallet.treasury is required")

	require.NoError(t, ValidateSolanaRPCURL(EnvironmentMainnet, "https://mainnet.helius-rpc.com/?api-key=k"))
	require.NoError(t, ValidateSolanaRPCURL(EnvironmentDevnet, "http://127.0.0.1:8899"))
	for environment, url := range map[string]string{EnvironmentMainnet: "http://127.0.0.1:8899/secret-path", EnvironmentDevnet: "http://rpc.example/secret-path"} {
		err := ValidateSolanaRPCURL(environment, url)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-path")
	}

	key := solanago.NewWallet().PrivateKey.String()
	local := SolanaCrankerConfig{Mode: CrankerLocalKeypair, PrivateKey: key}
	transit := SolanaCrankerConfig{Mode: CrankerVaultTransit, Key: "openrails-cranker"}
	// A cranker that cannot sign is turned off alone: it pulls nothing, and
	// the others serve.
	for name, row := range map[string]struct {
		crankers []SolanaCrankerConfig
		vault    *VaultConfig
		kept     []SolanaCrankerConfig
		refused  map[string]string
	}{
		"none":                {nil, nil, nil, nil},
		"local keypair":       {[]SolanaCrankerConfig{local}, nil, []SolanaCrankerConfig{local}, nil},
		"transit then local":  {[]SolanaCrankerConfig{transit, local}, &VaultConfig{}, []SolanaCrankerConfig{transit, local}, nil},
		"bad keypair":         {[]SolanaCrankerConfig{transit, {Mode: CrankerLocalKeypair, PrivateKey: "x"}}, &VaultConfig{}, []SolanaCrankerConfig{transit}, map[string]string{"solana.crankers[1]": "private_key is not a base58 Solana keypair; it pulls none of its plans"}},
		"transit needs vault": {[]SolanaCrankerConfig{transit, local}, nil, []SolanaCrankerConfig{local}, map[string]string{"solana.crankers[0]": "vault_transit needs vault; it pulls none of its plans; the next listed cranker publishes new plans"}},
		"unknown mode":        {[]SolanaCrankerConfig{{Mode: "hsm"}}, nil, nil, map[string]string{"solana.crankers[0]": "mode must be"}},
		"listed twice":        {[]SolanaCrankerConfig{local, transit, local}, &VaultConfig{}, []SolanaCrankerConfig{local, transit}, map[string]string{"solana.crankers[2]": "local_keypair:" + solanago.MustPrivateKeyFromBase58(key).PublicKey().String() + " is listed twice"}},
	} {
		cfg := &Config{Vault: row.vault}
		if row.crankers != nil {
			cfg.Solana = &SolanaConfig{Crankers: row.crankers}
		}
		refused := map[string]string{}
		for _, d := range Degrade(cfg) {
			if strings.HasPrefix(d.Field, "solana.") {
				refused[d.Field] = d.Err.Error()
				require.NotContains(t, d.Err.Error(), key, name)
			}
		}
		require.Equal(t, row.kept, SolanaCrankers(cfg), name)
		require.Len(t, refused, len(row.refused), name)
		for field, want := range row.refused {
			require.Contains(t, refused[field], want, name)
		}
	}
}

// A PSP document is flat, and its rail decides its fields: each refuses what
// the rail lacks, naming what it takes.
func TestPSPDocumentSchema(t *testing.T) {
	d, err := DecodePSPDocument(map[string]any{"rail": "solana", "environment": "devnet", "treasury": "T", "secrets": map[string]any{"rpc_url": "https://rpc.example/k"}})
	require.NoError(t, err)
	require.Equal(t, PSPDocument{Rail: "solana", Environment: "devnet", AccountID: "T", Secrets: map[string]string{"rpc_url": "https://rpc.example/k"}}, d)
	require.Equal(t, map[string]any{"rail": "solana", "environment": "devnet", "treasury": "T", "secrets": map[string]string{"rpc_url": "https://rpc.example/k"}}, d.Fields())

	d, err = DecodePSPDocument(map[string]any{"rail": "nmi", "gateway_id": "1", "tokenization_key": "tk", "custodian": "bt"})
	require.NoError(t, err)
	require.Equal(t, PSPDocument{Rail: "nmi", AccountID: "1", Custodian: "bt", Settings: map[string]any{"tokenization_key": "tk"}}, d)

	// CCBill names its account by its two numbers, joined into the account
	// its id and webhook path have always used.
	d, err = DecodePSPDocument(map[string]any{"rail": "ccbill", "client_accnum": "945280", "client_subacc": "0000"})
	require.NoError(t, err)
	require.Equal(t, "945280-0000", d.AccountID)
	require.Equal(t, map[string]any{"rail": "ccbill", "client_accnum": "945280", "client_subacc": "0000"}, d.Fields())

	for doc, want := range map[*map[string]any]string{
		{"rail": "solana", "account_id": "x"}:                         `a solana PSP has no "account_id" (solana takes rail, environment, archived, treasury, secrets)`,
		{"rail": "solana", "tokens": map[string]any{}}:                `a solana PSP has no "tokens"`,
		{"rail": "solana", "rpc_provider": "helius"}:                  `a solana PSP has no "rpc_provider"`,
		{"rail": "solana", "signer": map[string]any{}}:                `a solana PSP has no "signer"`,
		{"rail": "stripe", "treasury": "T"}:                           `a stripe PSP has no "treasury"`,
		{"rail": "nmi", "account_id": "1"}:                            `a nmi PSP has no "account_id" (nmi takes rail, environment, archived, gateway_id, custodian,`,
		{"rail": "ccbill", "account_id": "945280-0000"}:               `a ccbill PSP has no "account_id" (ccbill takes rail, environment, archived, client_accnum, client_subacc, secrets)`,
		{"rail": "nmi", "gateway_id": 1233466}:                        `gateway_id must be a string: quote it (gateway_id: "1233466")`,
		{"rail": "stripe", "custodian": "bt"}:                         `a stripe PSP has no "custodian"`,
		{"rail": "nmi", "settings": map[string]any{}}:                 `a nmi PSP has no "settings"`,
		{"rail": "ccbill", "tokenization_key": "x"}:                   `a ccbill PSP has no "tokenization_key"`,
		{"rail": "paypal"}:                                            `unknown rail "paypal"`,
		{"environment": "live"}:                                       "rail is required",
		{"rail": "nmi", "secrets": map[string]any{"security_key": 1}}: "secrets.security_key must be a string",
	} {
		_, err := DecodePSPDocument(*doc)
		require.ErrorContains(t, err, want)
	}
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
	u, err := url.Parse(DBConnectionString(c))
	require.NoError(t, err)
	require.Equal(t, "postgresql", u.Scheme)
	require.Equal(t, "db.internal:5432", u.Host)
	require.Equal(t, "/billing", u.Path)
	require.Equal(t, "open rails", u.User.Username())
	pw, _ := u.User.Password()
	require.Equal(t, "p@ss/w:rd?#", pw)
	require.Equal(t, "require", u.Query().Get("sslmode"), "TLS unless explicitly disabled")

	c.SSLMode = "disable"
	u, err = url.Parse(DBConnectionString(c))
	require.NoError(t, err)
	require.Equal(t, "disable", u.Query().Get("sslmode"))

	require.Equal(t, "postgres://x/y", DBConnectionString(&DBConfig{URL: "postgres://x/y", Host: "ignored", Port: "1", Database: "d", Username: "u"}))
	require.Empty(t, DBConnectionString(&DBConfig{Host: "h", Port: "1", Database: "d"}), "incomplete parts never guess")
}
