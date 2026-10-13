package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validConfig() *Config {
	return &Config{
		ProviderWriteMode: ProviderWriteModeFull,
		DB:                &DBConfig{URL: "postgresql://app:app_password@localhost:5434/openrails_db?sslmode=disable"},
		RateLimits:        DefaultRateLimits(), Captcha: DefaultCaptcha(),
	}
}

// Validate refuses only what the engine cannot serve without.
func TestValidateRefusesWhatCannotBeServed(t *testing.T) {
	require.NoError(t, Validate(validConfig()))
	for name, row := range map[string]struct {
		edit func(*Config)
		want string // "" = accepted
	}{
		"write mode omitted":     {func(c *Config) { c.ProviderWriteMode = "" }, ""},
		"write mode padded":      {func(c *Config) { c.ProviderWriteMode = " Limited " }, ""},
		"write mode typo":        {func(c *Config) { c.ProviderWriteMode = "redaonly" }, "must be one of full, limited, readonly"},
		"write mode legacy test": {func(c *Config) { c.ProviderWriteMode = "test" }, "must be one of full, limited, readonly"},
		"rate limits omitted":    {func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		"rate limits host-owned": {func(c *Config) { c.RateLimits, c.RateLimitsDisabled = nil, true }, ""},
		"db missing":             {func(c *Config) { c.DB = nil }, "database configuration is required"},
		"db url undeterminable":  {func(c *Config) { c.DB = &DBConfig{} }, "database URL could not be determined"},
		"db schema injection":    {func(c *Config) { c.Database.Schema = "bill;drop" }, "not a valid Postgres identifier"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			row.edit(cfg)
			err := Validate(cfg)
			if row.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, row.want)
			}
		})
	}
}

// An optional setting that would not validate is turned off, set back to its
// default or loses the entry, and named; a trust or allow list only shrinks.
// Validate then accepts what is left.
func TestDegradeTurnsOffWhatCannotBeServed(t *testing.T) {
	require.Empty(t, Degrade(validConfig()))
	for name, row := range map[string]struct {
		edit  func(*Config)
		field string // "" = served as written
		want  string
		after func(*testing.T, *Config)
	}{
		"per-address ceiling": {func(c *Config) {
			c.RateLimits = &RateLimitsConfig{"default": {RequestsPerMinute: 300}, "purchase": {RequestsPerMinute: 5}}
		},
			"rate_limits.default", "not a bucket", func(t *testing.T, c *Config) {
				require.Equal(t, RateLimitsConfig{"purchase": {RequestsPerMinute: 5}}, *c.RateLimits)
			}},
		"redis exclusive": {func(c *Config) { c.Redis = &RedisConfig{Addr: "cache:6379", URL: "redis://cache:6379"} }, "redis", "redis.addr and redis.url are exclusive",
			func(t *testing.T, c *Config) { require.Nil(t, c.Redis) }},
		"captcha disabled": {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile} }, "", "", nil},
		"captcha half pair": {func(c *Config) { c.Captcha.SecretKey = "secret" }, "captcha", "BOTH site_key and secret_key",
			func(t *testing.T, c *Config) { require.False(t, CaptchaEnabled(c.Captcha)) }},
		"captcha unsupported": {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: "recaptcha", SiteKey: "s", SecretKey: "k"} }, "captcha", "unsupported provider",
			func(t *testing.T, c *Config) { require.False(t, CaptchaEnabled(c.Captcha)) }},
		"reconcile typo": {func(c *Config) { c.CatalogReconciliationInterval = "30minutes" }, "catalog_reconciliation_interval", "the default 1h applies",
			func(t *testing.T, c *Config) { require.Empty(t, c.CatalogReconciliationInterval) }},
		"llm default provider": {func(c *Config) { c.LLM = &LLMConfig{APIKey: "k"} }, "", "", nil},
		"llm unknown provider": {func(c *Config) { c.LLM = &LLMConfig{Provider: "gemini", APIKey: "k"} }, "llm", `invalid llm.provider "gemini"`,
			func(t *testing.T, c *Config) { require.Nil(t, c.LLM) }},
		"llm relative base":      {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "api.openai.com/v1"} }, "llm", "invalid llm.base_url", nil},
		"llm non-http base":      {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "ftp://host/v1"} }, "llm", "invalid llm.base_url", nil},
		"llm plaintext loopback": {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "http://localhost:11434/v1"} }, "llm", "must use https", nil},
		"llm https compatible":   {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "https://api.groq.com/openai/v1"} }, "", "", nil},
		"trusted proxy typo": {func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8", "not-a-cidr"} }, "trusted_proxies", "invalid CIDR",
			func(t *testing.T, c *Config) { require.Equal(t, []string{"10.0.0.0/8"}, c.TrustedProxies) }},
		"trusted proxy bare ip":     {func(c *Config) { c.TrustedProxies = []string{"10.0.0.5"} }, "trusted_proxies", "invalid CIDR", nil},
		"trusted proxy everything":  {func(c *Config) { c.TrustedProxies = []string{"0.0.0.0/0"} }, "trusted_proxies", "matches every address", func(t *testing.T, c *Config) { require.Empty(t, c.TrustedProxies) }},
		"trusted proxy wide but /1": {func(c *Config) { c.TrustedProxies = []string{"0.0.0.0/1", "192.168.0.0/16"} }, "", "", nil},
		"cloudflare everything v6":  {func(c *Config) { c.CloudflareProxies = []string{"::/0"} }, "cloudflare_proxies", "matches every address", nil},
		"ccbill allowlist typo":     {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"definitely-not-a-cidr"} }, "ccbill_webhook_ip_allowlist", "invalid CIDR", nil},
		"ccbill allowlist open": {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"0.0.0.0/0", "127.0.0.1/32"} }, "ccbill_webhook_ip_allowlist", "matches every address",
			func(t *testing.T, c *Config) { require.Equal(t, []string{"127.0.0.1/32"}, c.CCBillWebhookIPAllowlist) }},
		"ccbill allowlist loopback": {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"127.0.0.1/32"} }, "", "", nil},
		"return origin plaintext":   {func(c *Config) { c.ReturnOrigins = []string{"http://shop.example"} }, "return_origins", "must use HTTPS", func(t *testing.T, c *Config) { require.Empty(t, c.ReturnOrigins) }},
		"return origin with path":   {func(c *Config) { c.ReturnOrigins = []string{"https://shop.example/app"} }, "return_origins", "without a path", nil},
		"secret overlap beyond week": {func(c *Config) { c.WebhookSecretOverlap = "169h" }, "webhook_secret_overlap", "the default 24h0m0s applies",
			func(t *testing.T, c *Config) { require.Empty(t, c.WebhookSecretOverlap) }},
		"hyperswitch plaintext": {func(c *Config) {
			c.HyperSwitch = &HyperSwitchConfig{APIBaseURL: "http://hs.example", SDKURL: "https://sdk.example"}
		}, "hyperswitch", "requires HTTPS",
			func(t *testing.T, c *Config) { require.Nil(t, c.HyperSwitch) }},
		"platform vault without kv": {func(c *Config) { c.PlatformVault = &PlatformVaultConfig{Kind: "basis_theory", TenantID: "t"} }, "platform_vault", "needs vault.kv_mount",
			func(t *testing.T, c *Config) { require.Nil(t, c.PlatformVault) }},
		"sandbox gateway not loopback": {func(c *Config) {
			c.ProviderSandbox = &ProviderSandboxConfig{NMIGatewayURL: "http://10.0.0.5:9", StripeAPIURL: "http://127.0.0.1:2"}
		}, "provider_sandbox.nmi_gateway_url", "not a loopback address",
			func(t *testing.T, c *Config) {
				require.Equal(t, ProviderSandboxConfig{StripeAPIURL: "http://127.0.0.1:2"}, *c.ProviderSandbox)
			}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			row.edit(cfg)
			degraded := Degrade(cfg)
			require.NoError(t, Validate(cfg))
			if row.field == "" {
				require.Empty(t, degraded)
				return
			}
			require.Len(t, degraded, 1, "%v", degraded)
			require.Equal(t, row.field, degraded[0].Field)
			require.ErrorContains(t, degraded[0].Err, row.want)
			require.Empty(t, Degrade(cfg), "what is left serves")
			if row.after != nil {
				row.after(t, cfg)
			}
		})
	}
	host := validConfig()
	host.SMTP = &SMTPConfig{Host: "smtp.example"}
	require.Equal(t, "smtp", DegradeEmail(host, true)[0].Field)
	require.Nil(t, host.SMTP, "the host's sender wins")
	hostless := validConfig()
	hostless.SMTP = &SMTPConfig{}
	require.ErrorContains(t, DegradeEmail(hostless, false)[0].Err, "Config.SMTP.Host is required")
	require.Nil(t, hostless.SMTP)
}

// Mount paths land in route patterns and the console's <base href> (#1127).
func TestValidateMountPath(t *testing.T) {
	for path, ok := range map[string]bool{
		"/admin": true, "/billing/admin": true, "/a.b/c~d/e_f-g": true,
		"": false, "/": false, "admin": false, "/admin/": false, "//admin": false, "/a//b": false,
		"/a/../b": false, "/./a": false, "/..": false, "/a b": false, "/a%2Fb": false,
		`/x"><script>`: false, "/a?b": false, "/a#b": false, "/{x}": false, " /admin": false,
	} {
		if err := ValidateMountPath("admin_console.path", path); ok {
			require.NoError(t, err, path)
		} else {
			require.ErrorContains(t, err, "invalid admin_console.path", path)
		}
	}
}

func TestPostureAccessorsFailClosed(t *testing.T) {
	for _, row := range []struct {
		mode              string
		limited, readonly bool
	}{
		{"", true, true}, {ProviderWriteModeFull, false, false}, {ProviderWriteModeLimited, true, false},
		{ProviderWriteModeReadOnly, true, true}, {" Limited ", true, false}, {"redaonly", true, true},
	} {
		cfg := &Config{ProviderWriteMode: row.mode}
		require.Equal(t, row.limited, IsLimitedMode(cfg), row.mode)
		require.Equal(t, row.readonly, IsProviderReadOnly(cfg), row.mode)
	}

	var nilCfg *Config
	require.Empty(t, MerchantConfigKVMount(nilCfg))
	require.Empty(t, MerchantConfigKVMount(&Config{Vault: &VaultConfig{TransitMount: "transit"}}), "a Transit-only Vault only signs")
	require.Equal(t, "kv", MerchantConfigKVMount(&Config{Vault: &VaultConfig{KVMount: " /kv/ "}}))
}

func TestScalarParsingBoundaries(t *testing.T) {
	for raw, want := range map[string]struct {
		interval time.Duration
		enabled  bool
	}{"": {time.Hour, true}, "30m": {30 * time.Minute, true}, "0": {0, false}, "-5m": {0, false}} {
		interval, enabled, err := CatalogReconciliationSchedule(&Config{CatalogReconciliationInterval: raw})
		require.NoError(t, err)
		require.Equal(t, want.interval, interval, raw)
		require.Equal(t, want.enabled, enabled, raw)
	}

	for raw, want := range map[string]string{"": DefaultSchema, "  Custom_Billing  ": "custom_billing"} {
		require.Equal(t, want, SchemaName(&Config{Database: DatabaseConfig{Schema: raw}}))
	}
	require.Equal(t, DefaultSchema, SchemaName((*Config)(nil)))
	for _, raw := range []string{"1schema", "bad schema", "bad-schema", `"quoted"`, "a.b"} {
		require.Error(t, validateSchema(raw), raw)
	}

	for cfg, model := range map[LLMConfig]string{
		{}: LLMDefaultModelAnthropic, {Provider: "openai"}: LLMDefaultModelOpenAI, {Provider: "openai", Model: " custom "}: "custom",
	} {
		require.Equal(t, model, LLMModel(&cfg))
	}
}

// Sandbox gateways receive provider credentials, so the destination must be a
// literal loopback IP (no resolver); any other is dropped. They are fake
// providers, which a live PSP never sits beside.
func TestProviderSandboxGatewaysAreLiteralLoopbackOnly(t *testing.T) {
	validate := func(sandbox ProviderSandboxConfig) error {
		cfg := validConfig()
		cfg.ProviderSandbox = &sandbox
		if degraded := Degrade(cfg); len(degraded) > 0 {
			return degraded[0].Err
		}
		return nil
	}
	both := func(dest string) map[string]ProviderSandboxConfig {
		return map[string]ProviderSandboxConfig{"stripe": {StripeAPIURL: dest}, "nmi": {NMIGatewayURL: dest}}
	}
	for _, dest := range []string{
		"http://127.0.0.1:9092", "https://127.0.0.1", "http://127.0.0.1:9092/v1", "http://127.255.0.7:80",
		"http://[::1]:9092", "http://[::ffff:127.0.0.1]:9092", " http://127.0.0.1:9092 ",
	} {
		for key, sandbox := range both(dest) {
			require.NoError(t, validate(sandbox), "%s %q", key, dest)
		}
	}
	for _, dest := range []string{
		"http://localhost:9092", "https://api.stripe.com", "http://10.0.0.5:9092", "http://169.254.169.254",
		"http://93.184.216.34", "http://[2001:db8::1]:9092", "http://0.0.0.0:9092", "http://user:pass@127.0.0.1:9092",
		"http://@127.0.0.1:9092", "ftp://127.0.0.1:9092", "127.0.0.1:9092", "http:///v1", "http://[::1",
	} {
		for key, sandbox := range both(dest) {
			require.ErrorIs(t, validate(sandbox), ErrProviderSandboxGateway, "%s %q", key, dest)
		}
	}
	require.NoError(t, validate(ProviderSandboxConfig{}))
	cfg := validConfig()
	cfg.ProviderSandbox = &ProviderSandboxConfig{NMIGatewayURL: "http://127.0.0.1:1", StripeAPIURL: "http://127.0.0.1:2"}
	cfg.HyperSwitch = &HyperSwitchConfig{AllowLoopbackHTTP: true}
	require.Equal(t, []string{"hyperswitch.allow_loopback_http", "provider_sandbox.nmi_gateway_url", "provider_sandbox.stripe_api_url"}, FakeProviders(cfg))
	require.Empty(t, FakeProviders(validConfig()))
}

// Each rail takes only its own environments, its real-money one when a PSP
// omits it, and a PSP agrees with its merchant: live or a sandbox.
func TestPSPEnvironments(t *testing.T) {
	all := []string{EnvironmentLive, EnvironmentTest, EnvironmentSandbox, EnvironmentMainnet, EnvironmentDevnet, EnvironmentTestnet}
	for rail, environments := range map[string][]string{
		"nmi":    {EnvironmentLive, EnvironmentTest, EnvironmentSandbox},
		"stripe": {EnvironmentLive, EnvironmentTest},
		"ccbill": {EnvironmentLive, EnvironmentSandbox},
		"solana": {EnvironmentMainnet, EnvironmentDevnet, EnvironmentTestnet},
	} {
		for _, environment := range all {
			got, err := ResolvePSPEnvironment("p", rail, environment)
			if slices.Contains(environments, environment) {
				require.NoError(t, err, "%s %s", rail, environment)
				require.Equal(t, environment, got)
				require.Equal(t, environment == environments[0], LiveEnvironment(rail, environment), "%s %s", rail, environment)
			} else {
				require.EqualError(t, err, "psps.p.environment: "+rail+` has no "`+environment+`" environment (`+strings.Join(environments, ", ")+")")
			}
		}
		got, err := ResolvePSPEnvironment("p", rail, "")
		require.NoError(t, err)
		require.Equal(t, environments[0], got, "%s defaults to its real-money environment", rail)
		require.Equal(t, ClassLive, EnvironmentClass(rail, environments[0]))
		require.Equal(t, ClassTest, EnvironmentClass(rail, environments[len(environments)-1]))
	}
	// A merchant's PSPs agree with it: real money for a live merchant, none for
	// a sandbox.
	for _, row := range []struct {
		rail, environment string
		sandbox           bool
		want              string
	}{
		{"nmi", EnvironmentTest, true, ""},
		{"solana", EnvironmentDevnet, true, ""},
		{"solana", EnvironmentMainnet, false, ""},
		{"stripe", EnvironmentLive, false, ""},
		{"solana", EnvironmentMainnet, true, "psps.p.environment: mainnet moves real money, and the merchant is a sandbox (sandbox: true); its PSPs take devnet or testnet"},
		{"nmi", EnvironmentSandbox, false, "psps.p.environment: sandbox moves no money, and the merchant is live; a sandbox is its own merchant, declared sandbox: true"},
	} {
		err := ValidatePSPMoneyClass("p", row.rail, row.environment, row.sandbox)
		if row.want == "" {
			require.NoError(t, err, "%+v", row)
		} else {
			require.EqualError(t, err, row.want)
		}
	}
}

// The custody deployment is host-owned: secure transport only (any other is
// turned off), and no tenant setting can redirect it.
func TestHyperSwitchEndpointsAreHostOwnedAndSecure(t *testing.T) {
	served := func(cfg *Config) bool { return len(Degrade(cfg)) == 0 && cfg.HyperSwitch != nil }
	for _, row := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"https", "https://vault.example.test", true},
		{"loopback fixture", "http://127.0.0.1:9099", true},
		{"private http", "http://10.0.0.1", false},
		{"hostname http", "http://localhost", false},
		{"userinfo", "https://user:secret@vault.example.test", false},
		{"query", "https://vault.example.test?token=secret", false},
		{"fragment", "https://vault.example.test#secret", false},
		{"missing host", "https:///vault", false},
		{"missing", "", false},
	} {
		cfg := validConfig()
		cfg.HyperSwitch = &HyperSwitchConfig{AllowLoopbackHTTP: true, APIBaseURL: row.url, SDKURL: row.url}
		require.Equal(t, row.ok, served(cfg), row.name)
	}
	cfg := validConfig()
	cfg.HyperSwitch = &HyperSwitchConfig{APIBaseURL: "https://vault.example.test", SDKURL: "https://sdk.example.test"}
	require.True(t, served(cfg))
	cfg.HyperSwitch.AllowLoopbackHTTP = false
	cfg.HyperSwitch.SDKURL = "http://127.0.0.1:9099"
	require.False(t, served(cfg), "loopback HTTP needs the explicit exception")
}
