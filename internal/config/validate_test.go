package config

import (
	"slices"
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

func TestValidateRefusesUnsafeConfiguration(t *testing.T) {
	require.NoError(t, Validate(validConfig()))
	for name, row := range map[string]struct {
		edit func(*Config)
		want string // "" = accepted
	}{
		"write mode omitted":         {func(c *Config) { c.ProviderWriteMode = "" }, ""},
		"write mode padded":          {func(c *Config) { c.ProviderWriteMode = " Limited " }, ""},
		"write mode typo":            {func(c *Config) { c.ProviderWriteMode = "redaonly" }, "must be one of full, limited, readonly"},
		"write mode legacy test":     {func(c *Config) { c.ProviderWriteMode = "test" }, "must be one of full, limited, readonly"},
		"rate limits omitted":        {func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		"rate limits host-owned":     {func(c *Config) { c.RateLimits, c.RateLimitsDisabled = nil, true }, ""},
		"per-address ceiling":        {func(c *Config) { c.RateLimits = &RateLimitsConfig{"default": {RequestsPerMinute: 300}} }, "rate_limits.default is not a bucket"},
		"captcha disabled":           {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile} }, ""},
		"captcha half pair":          {func(c *Config) { c.Captcha.SecretKey = "secret" }, "BOTH site_key and secret_key"},
		"captcha unsupported":        {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: "recaptcha", SiteKey: "s", SecretKey: "k"} }, "unsupported provider"},
		"reconcile typo":             {func(c *Config) { c.CatalogReconciliationInterval = "30minutes" }, "catalog_reconciliation_interval"},
		"db missing":                 {func(c *Config) { c.DB = nil }, "database configuration is required"},
		"db url undeterminable":      {func(c *Config) { c.DB = &DBConfig{} }, "database URL could not be determined"},
		"db schema injection":        {func(c *Config) { c.Database.Schema = "bill;drop" }, "not a valid Postgres identifier"},
		"vault kv mount":             {func(c *Config) { c.Vault = &VaultConfig{KVMount: "kv"} }, ""},
		"vault kv mount escapes":     {func(c *Config) { c.Vault = &VaultConfig{KVMount: "kv/../sys"} }, "plain Vault path"},
		"vault transit only":         {func(c *Config) { c.Vault = &VaultConfig{TransitMount: "transit"} }, ""},
		"vault auth mount":           {func(c *Config) { c.Vault = &VaultConfig{AuthMethod: "kubernetes", AuthMount: "openrails-prod/k8s"} }, ""},
		"vault auth mount with auth": {func(c *Config) { c.Vault = &VaultConfig{AuthMethod: "approle", AuthMount: "auth/approle2"} }, "the path after auth/"},
		"vault auth mount for token": {func(c *Config) { c.Vault = &VaultConfig{Token: "t", AuthMount: "k8s"} }, `not "token"`},
		"llm default provider":       {func(c *Config) { c.LLM = &LLMConfig{APIKey: "k"} }, ""},
		"llm unknown provider":       {func(c *Config) { c.LLM = &LLMConfig{Provider: "gemini", APIKey: "k"} }, `invalid llm.provider "gemini"`},
		"llm relative base":          {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "api.openai.com/v1"} }, "invalid llm.base_url"},
		"llm non-http base":          {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "ftp://host/v1"} }, "invalid llm.base_url"},
		"llm plaintext loopback":     {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "http://localhost:11434/v1"} }, "must use https"},
		"llm https compatible":       {func(c *Config) { c.LLM = &LLMConfig{Provider: "openai", BaseURL: "https://api.groq.com/openai/v1"} }, ""},
		"trusted proxy typo":         {func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8", "not-a-cidr"} }, "trusted_proxies"},
		"trusted proxy bare ip":      {func(c *Config) { c.TrustedProxies = []string{"10.0.0.5"} }, "trusted_proxies"},
		"trusted proxy everything":   {func(c *Config) { c.TrustedProxies = []string{"0.0.0.0/0"} }, "matches every address"},
		"trusted proxy wide but /1":  {func(c *Config) { c.TrustedProxies = []string{"0.0.0.0/1", "192.168.0.0/16"} }, ""},
		"cloudflare everything v6":   {func(c *Config) { c.CloudflareProxies = []string{"::/0"} }, "cloudflare_proxies"},
		"ccbill allowlist typo":      {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"definitely-not-a-cidr"} }, "ccbill_webhook_ip_allowlist"},
		"ccbill allowlist open":      {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"0.0.0.0/0"} }, "ccbill_webhook_ip_allowlist"},
		"ccbill allowlist loopback":  {func(c *Config) { c.CCBillWebhookIPAllowlist = []string{"127.0.0.1/32"} }, ""},
		"return origin plaintext":    {func(c *Config) { c.ReturnOrigins = []string{"http://shop.example"} }, "must use HTTPS"},
		"return origin with path":    {func(c *Config) { c.ReturnOrigins = []string{"https://shop.example/app"} }, "without a path"},
		"secret overlap beyond week": {func(c *Config) { c.WebhookSecretOverlap = "169h" }, "webhook"},
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
// literal loopback IP (no resolver); they are fake providers, which a live PSP
// never sits beside.
func TestProviderSandboxGatewaysAreLiteralLoopbackOnly(t *testing.T) {
	validate := func(sandbox ProviderSandboxConfig) error {
		cfg := validConfig()
		cfg.ProviderSandbox = &sandbox
		return Validate(cfg)
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

// Each rail takes only the environments it has, every PSP declares one, and a
// deployment's PSPs are all live or all non-live.
func TestPSPEnvironments(t *testing.T) {
	for rail, environments := range map[string][]string{
		"nmi":    {EnvironmentLive, EnvironmentTest, EnvironmentSandbox},
		"stripe": {EnvironmentLive, EnvironmentTest},
		"ccbill": {EnvironmentLive, EnvironmentSandbox},
		"solana": {EnvironmentLive, EnvironmentSandbox},
	} {
		for _, environment := range []string{EnvironmentLive, EnvironmentTest, EnvironmentSandbox} {
			err := ValidatePSPEnvironment("p", rail, environment)
			if slices.Contains(environments, environment) {
				require.NoError(t, err, "%s %s", rail, environment)
			} else {
				require.ErrorContains(t, err, "psps.p.environment: "+rail+` has no "`+environment+`" environment`)
			}
		}
		require.ErrorContains(t, ValidatePSPEnvironment("p", rail, ""), "psps.p.environment is required")
		require.Error(t, ValidatePSPEnvironment("p", rail, "production"), "no aliases")
	}
	require.NoError(t, ValidateDeploymentEnvironments(map[string]string{"a/nmi": EnvironmentTest, "a/solana": EnvironmentSandbox, "b/stripe": EnvironmentTest}))
	require.NoError(t, ValidateDeploymentEnvironments(map[string]string{"a/nmi": EnvironmentLive, "b/stripe": EnvironmentLive}))
	require.EqualError(t, ValidateDeploymentEnvironments(map[string]string{"a/nmi": EnvironmentLive, "b/solana": EnvironmentSandbox}),
		"a deployment is all live or all non-live: live PSPs a/nmi beside non-live PSPs b/solana (sandbox)")
	require.Equal(t, "devnet", SolanaNetwork(EnvironmentSandbox))
	require.Equal(t, "mainnet", SolanaNetwork(EnvironmentLive))
	require.Equal(t, EnvironmentTest, EnvironmentClass(EnvironmentSandbox))
	require.Equal(t, EnvironmentLive, EnvironmentClass(EnvironmentLive))
}

// The custody deployment is host-owned: secure transport only, and no tenant
// setting can redirect it.
func TestHyperSwitchEndpointsAreHostOwnedAndSecure(t *testing.T) {
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
		require.Equal(t, row.ok, Validate(cfg) == nil, row.name)
	}
	cfg := validConfig()
	cfg.HyperSwitch = &HyperSwitchConfig{APIBaseURL: "https://vault.example.test", SDKURL: "https://sdk.example.test"}
	require.NoError(t, Validate(cfg))
	cfg.HyperSwitch.AllowLoopbackHTTP = false
	cfg.HyperSwitch.SDKURL = "http://127.0.0.1:9099"
	require.Error(t, Validate(cfg), "loopback HTTP needs the explicit exception")
}
