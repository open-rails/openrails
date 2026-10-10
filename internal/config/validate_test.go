package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validConfig() *Config {
	return &Config{
		TestMode: CredentialPostureSandbox, ProviderWriteMode: ProviderWriteModeFull,
		DB:         &DBConfig{URL: "postgresql://app:app_password@localhost:5434/openrails_db?sslmode=disable"},
		RateLimits: DefaultRateLimits(), Captcha: DefaultCaptcha(),
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
		"posture live":               {func(c *Config) { c.TestMode = CredentialPostureLive }, ""},
		"posture omitted":            {func(c *Config) { c.TestMode = "" }, "test_mode is required"},
		"posture garbage":            {func(c *Config) { c.TestMode = "yes" }, `invalid test_mode "yes"`},
		"rate limits omitted":        {func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		"rate limits host-owned":     {func(c *Config) { c.RateLimits, c.RateLimitsDisabled = nil, true }, ""},
		"per-address ceiling":        {func(c *Config) { c.RateLimits = &RateLimitsConfig{"default": {RequestsPerMinute: 300}} }, "rate_limits.default is not a bucket"},
		"captcha disabled":           {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile} }, ""},
		"captcha half pair":          {func(c *Config) { c.Captcha.SecretKey = "secret" }, "BOTH site_key and secret_key"},
		"captcha unsupported":        {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: "recaptcha", SiteKey: "s", SecretKey: "k"} }, "unsupported provider"},
		"quiescence sub-second":      {func(c *Config) { c.ProviderBillingQuiescenceInterval = "500ms" }, "at least one second"},
		"quiescence fractional":      {func(c *Config) { c.ProviderBillingQuiescenceInterval = "1500ms" }, "whole seconds"},
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
	require.Equal(t, "/admin", AdminConsolePath(&ConsoleMount{}))
	require.Equal(t, "/ops", AdminConsolePath(&ConsoleMount{Path: "/ops"}))
}

// The host's extension data reaches the console through config.json, keyed by
// extension id.
func TestValidateConsoleExtensions(t *testing.T) {
	for _, row := range []struct {
		extensions map[string]any
		err        string
	}{
		{nil, ""},
		{map[string]any{"hosted": map[string]any{"plans": []any{"starter"}}}, ""},
		{map[string]any{"saas-2": true}, ""},
		{map[string]any{"Hosted": true}, `invalid Routes.AdminConsole.Extensions key "Hosted"`},
		{map[string]any{"-x": true}, `invalid Routes.AdminConsole.Extensions key "-x"`},
		{map[string]any{"x": func() {}}, "invalid Routes.AdminConsole.Extensions.x"},
	} {
		if err := ValidateConsoleExtensions("Routes.AdminConsole.Extensions", row.extensions); row.err == "" {
			require.NoError(t, err, row.extensions)
		} else {
			require.ErrorContains(t, err, row.err)
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
		for _, posture := range []CredentialPosture{"", CredentialPostureLive, CredentialPostureSandbox} {
			cfg := &Config{TestMode: posture, ProviderWriteMode: row.mode}
			require.Equal(t, posture == CredentialPostureSandbox, IsTestMode(cfg))
			require.Equal(t, row.limited, IsLimitedMode(cfg), row.mode)
			require.Equal(t, row.readonly, IsProviderReadOnly(cfg), row.mode)
		}
	}
	require.Equal(t, ProviderEnvironmentTest, ExpectedProviderEnvironment(true))
	require.Equal(t, ProviderEnvironmentLive, ExpectedProviderEnvironment(false))

	var nilCfg *Config
	require.Empty(t, MerchantConfigKVMount(nilCfg))
	require.Empty(t, MerchantConfigKVMount(&Config{Vault: &VaultConfig{TransitMount: "transit"}}), "a Transit-only Vault only signs")
	require.Equal(t, "kv", MerchantConfigKVMount(&Config{Vault: &VaultConfig{KVMount: " /kv/ "}}))
}

func TestScalarParsingBoundaries(t *testing.T) {
	for raw, want := range map[string]CredentialPosture{"": "", " SANDBOX ": CredentialPostureSandbox, "live": CredentialPostureLive} {
		got, err := ParseCredentialPosture(raw)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := ParseCredentialPosture("true")
	require.Error(t, err, "a bool-shaped posture must never decode")

	for raw, want := range map[string]time.Duration{"": 24 * time.Hour, "36h": 36 * time.Hour} {
		got, err := ProviderBillingQuiescence(&Config{ProviderBillingQuiescenceInterval: raw})
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"0", "-1h", "tomorrow"} {
		_, err := ProviderBillingQuiescence(&Config{ProviderBillingQuiescenceInterval: raw})
		require.Error(t, err, raw)
	}

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
// literal loopback IP (no resolver) and never under a live posture.
func TestProviderSandboxGatewaysAreLiteralLoopbackOnly(t *testing.T) {
	validate := func(posture CredentialPosture, sandbox ProviderSandboxConfig) error {
		cfg := validConfig()
		cfg.TestMode, cfg.ProviderSandbox = posture, &sandbox
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
			require.NoError(t, validate(CredentialPostureSandbox, sandbox), "%s %q", key, dest)
		}
	}
	for _, dest := range []string{
		"http://localhost:9092", "https://api.stripe.com", "http://10.0.0.5:9092", "http://169.254.169.254",
		"http://93.184.216.34", "http://[2001:db8::1]:9092", "http://0.0.0.0:9092", "http://user:pass@127.0.0.1:9092",
		"http://@127.0.0.1:9092", "ftp://127.0.0.1:9092", "127.0.0.1:9092", "http:///v1", "http://[::1",
	} {
		for key, sandbox := range both(dest) {
			require.ErrorIs(t, validate(CredentialPostureSandbox, sandbox), ErrProviderSandboxGateway, "%s %q", key, dest)
		}
	}
	for key, sandbox := range both("http://127.0.0.1:9092") {
		require.ErrorIs(t, validate(CredentialPostureLive, sandbox), ErrProviderSandboxGateway, "%s under live", key)
	}
	require.NoError(t, validate(CredentialPostureLive, ProviderSandboxConfig{}))
}

// The custody deployment is host-owned: secure transport only, and no tenant
// setting can redirect it.
func TestHyperSwitchEndpointsAreHostOwnedAndSecure(t *testing.T) {
	for _, row := range []struct {
		name    string
		posture CredentialPosture
		url     string
		ok      bool
	}{
		{"https", CredentialPostureLive, "https://vault.example.test", true},
		{"loopback fixture", CredentialPostureSandbox, "http://127.0.0.1:9099", true},
		{"loopback under live", CredentialPostureLive, "http://127.0.0.1:9099", false},
		{"private http", CredentialPostureSandbox, "http://10.0.0.1", false},
		{"hostname http", CredentialPostureSandbox, "http://localhost", false},
		{"userinfo", CredentialPostureSandbox, "https://user:secret@vault.example.test", false},
		{"query", CredentialPostureSandbox, "https://vault.example.test?token=secret", false},
		{"fragment", CredentialPostureSandbox, "https://vault.example.test#secret", false},
		{"missing host", CredentialPostureSandbox, "https:///vault", false},
		{"missing", CredentialPostureSandbox, "", false},
	} {
		cfg := validConfig()
		cfg.TestMode = row.posture
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
