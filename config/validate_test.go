package config

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testMasterKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

func validConfig() *Config {
	cfg := GetDefaultBillingConfig()
	cfg.TestMode = CredentialPostureSandbox
	cfg.ProviderWriteMode = ProviderWriteModeFull
	assembleDBURL(cfg)
	return cfg
}

func TestValidateRefusesUnsafeConfiguration(t *testing.T) {
	require.NoError(t, Validate(validConfig()))
	for name, row := range map[string]struct {
		edit func(*Config)
		want string // "" = accepted
	}{
		"write mode omitted":     {func(c *Config) { c.ProviderWriteMode = "" }, ""},
		"write mode padded":      {func(c *Config) { c.ProviderWriteMode = " Limited " }, ""},
		"write mode typo":        {func(c *Config) { c.ProviderWriteMode = "redaonly" }, "must be one of full, limited, readonly"},
		"write mode legacy test": {func(c *Config) { c.ProviderWriteMode = "test" }, "must be one of full, limited, readonly"},
		"posture live":           {func(c *Config) { c.TestMode = CredentialPostureLive }, ""},
		"posture omitted":        {func(c *Config) { c.TestMode = "" }, "test_mode is required"},
		"posture garbage":        {func(c *Config) { c.TestMode = "yes" }, `invalid test_mode "yes"`},
		"rate limits omitted":    {func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		"rate limits host-owned": {func(c *Config) { c.RateLimits, c.RateLimitsDisabled = nil, true }, ""},
		"captcha disabled":       {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile} }, ""},
		"captcha half pair":      {func(c *Config) { c.Captcha.SecretKey = "secret" }, "BOTH site_key and secret_key"},
		"captcha unsupported":    {func(c *Config) { c.Captcha = &CaptchaConfig{Provider: "recaptcha", SiteKey: "s", SecretKey: "k"} }, "unsupported provider"},
		"port ephemeral range":   {func(c *Config) { c.Port = 44553 }, ""},
		"port above range":       {func(c *Config) { c.Port = 70000 }, "invalid port"},
		"port int16 wrap":        {func(c *Config) { c.Port = -20983 }, "invalid port"},
		"quiescence sub-second":  {func(c *Config) { c.ProviderBillingQuiescenceInterval = "500ms" }, "at least one second"},
		"quiescence fractional":  {func(c *Config) { c.ProviderBillingQuiescenceInterval = "1500ms" }, "whole seconds"},
		"reconcile typo":         {func(c *Config) { c.CatalogReconciliationInterval = "30minutes" }, "catalog_reconciliation_interval"},
		"default db user":        {func(c *Config) { c.DB.Username = " admin " }, "default database credentials"},
		"default db password":    {func(c *Config) { c.DB.Password = "admin_password" }, "default database credentials"},
		"db missing":             {func(c *Config) { c.DB = nil }, "database configuration is required"},
		"db url undeterminable":  {func(c *Config) { c.DB = &DBConfig{} }, "database URL could not be determined"},
		"db schema injection":    {func(c *Config) { c.DB.Schema = "bill;drop" }, "not a valid Postgres identifier"},
		"secret backend unknown": {func(c *Config) { c.SecretBackend = "consul" }, "secret_backend must be snapshot, db or vault"},
		"db custody without key": {func(c *Config) { c.SecretBackend = SecretBackendDB }, "encryption.master_key"},
		"db custody with key": {func(c *Config) {
			c.SecretBackend, c.Encryption = SecretBackendDB, &EncryptionConfig{MasterKey: testMasterKey}
		}, ""},
		"alert db without key":  {func(c *Config) { c.AlertSecretBackend = SecretBackendDB }, "encryption.master_key"},
		"alert snapshot":        {func(c *Config) { c.AlertSecretBackend = SecretBackendSnapshot }, "alert_secret_backend must be db or vault"},
		"vault custody":         {func(c *Config) { c.SecretBackend, c.Vault = SecretBackendVault, &VaultConfig{Enabled: true} }, ""},
		"snapshot id canonical": {func(c *Config) { c.CredentialSnapshotID = "4b1c9f0e-2a3d-4e5f-8a9b-0c1d2e3f4a5b" }, ""},
		"snapshot id uppercase": {func(c *Config) { c.CredentialSnapshotID = "4B1C9F0E-2A3D-4E5F-8A9B-0C1D2E3F4A5B" }, "canonical nonzero UUID"},
		"snapshot id nil":       {func(c *Config) { c.CredentialSnapshotID = "00000000-0000-0000-0000-000000000000" }, "canonical nonzero UUID"},
		"master key not base64": {func(c *Config) { c.Encryption = &EncryptionConfig{MasterKey: "not!base64!"} }, "must be valid base64"},
		"master key short": {func(c *Config) {
			c.Encryption = &EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 16))}
		}, "must decode to 32 bytes"},
		"master key blank":           {func(c *Config) { c.Encryption = &EncryptionConfig{MasterKey: "   "} }, ""},
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
			require.Equal(t, posture == CredentialPostureSandbox, cfg.IsTestMode())
			require.Equal(t, row.limited, cfg.IsLimitedMode(), row.mode)
			require.Equal(t, row.readonly, cfg.IsProviderReadOnly(), row.mode)
			require.True(t, cfg.RequiresSecretEncryption())
		}
	}
	require.Equal(t, ProviderEnvironmentTest, ExpectedProviderEnvironment(true))
	require.Equal(t, ProviderEnvironmentLive, ExpectedProviderEnvironment(false))

	var nilCfg *Config
	require.Equal(t, SecretBackendSnapshot, nilCfg.SecretStoreBackend())
	require.Equal(t, SecretBackendVault, (&Config{SecretBackend: " Vault "}).SecretStoreBackend())
	require.False(t, GetDefaultBillingConfig().AllowCatalogUpdates)
}

func TestScalarParsingBoundaries(t *testing.T) {
	for raw, want := range map[string]FlexiblePort{"44553": 44553, "65535": 65535, " 3053 ": 3053, "": 0} {
		var got FlexiblePort
		require.NoError(t, got.UnmarshalText([]byte(raw)))
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"65536", "0", "-1", "not-a-port"} {
		var got FlexiblePort
		require.Error(t, got.UnmarshalText([]byte(raw)), raw)
	}

	for raw, want := range map[string]CredentialPosture{"": "", " SANDBOX ": CredentialPostureSandbox, "live": CredentialPostureLive} {
		var got CredentialPosture
		require.NoError(t, got.UnmarshalText([]byte(raw)))
		require.Equal(t, want, got)
	}
	var posture CredentialPosture
	require.Error(t, posture.UnmarshalText([]byte("true")), "a bool-shaped posture must never decode")

	for raw, want := range map[string]time.Duration{"": 24 * time.Hour, "36h": 36 * time.Hour} {
		got, err := (&Config{ProviderBillingQuiescenceInterval: raw}).ProviderBillingQuiescence()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"0", "-1h", "tomorrow"} {
		_, err := (&Config{ProviderBillingQuiescenceInterval: raw}).ProviderBillingQuiescence()
		require.Error(t, err, raw)
	}

	for raw, want := range map[string]struct {
		interval time.Duration
		enabled  bool
	}{"": {time.Hour, true}, "30m": {30 * time.Minute, true}, "0": {0, false}, "-5m": {0, false}} {
		interval, enabled, err := (&Config{CatalogReconciliationInterval: raw}).CatalogReconciliationSchedule()
		require.NoError(t, err)
		require.Equal(t, want.interval, interval, raw)
		require.Equal(t, want.enabled, enabled, raw)
	}

	for raw, want := range map[string]string{"": DefaultSchema, "  Custom_Billing  ": "custom_billing"} {
		require.Equal(t, want, (&DBConfig{Schema: raw}).SchemaName())
	}
	require.Equal(t, DefaultSchema, (*DBConfig)(nil).SchemaName())
	for _, raw := range []string{"1schema", "bad schema", "bad-schema", `"quoted"`, "a.b"} {
		require.Error(t, validateSchema(raw), raw)
	}

	for cfg, model := range map[LLMConfig]string{
		{}: LLMDefaultModelAnthropic, {Provider: "openai"}: LLMDefaultModelOpenAI, {Provider: "openai", Model: " custom "}: "custom",
	} {
		require.Equal(t, model, cfg.ResolvedModel())
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
		cfg.TestMode, cfg.Encryption = row.posture, &EncryptionConfig{MasterKey: testMasterKey}
		cfg.HyperSwitch = &HyperSwitchConfig{AllowLoopbackHTTP: true, APIBaseURL: row.url, SDKURL: row.url}
		require.Equal(t, row.ok, Validate(cfg) == nil, row.name)
	}
	cfg := validConfig()
	cfg.HyperSwitch = &HyperSwitchConfig{APIBaseURL: "https://vault.example.test", SDKURL: "https://sdk.example.test"}
	require.ErrorContains(t, Validate(cfg), "requires encryption.master_key")
	cfg.HyperSwitch.AllowLoopbackHTTP, cfg.Encryption = false, &EncryptionConfig{MasterKey: testMasterKey}
	cfg.HyperSwitch.SDKURL = "http://127.0.0.1:9099"
	require.Error(t, Validate(cfg), "loopback HTTP needs the explicit exception")
}
