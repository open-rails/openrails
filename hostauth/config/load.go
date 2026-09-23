package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-viper/mapstructure/v2"
	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	billing "github.com/open-rails/openrails/config"
	log "github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func loadConfigIfExists(k *koanf.Koanf, path string) error {
	if path == "" {
		return nil
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		candidates = append(candidates, filepath.Join("config", path))
		candidates = append(candidates, filepath.Join("./config", path))
	}
	visited := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := visited[candidate]; ok {
			continue
		}
		visited[candidate] = struct{}{}
		// Normalize the path before touching the filesystem. The path is
		// operator-supplied (CLI flag / env var / built-in default), but cleaning
		// it removes any "../" traversal segments defensively.
		cleaned := filepath.Clean(candidate)
		if _, err := os.Stat(cleaned); err == nil {
			if err := k.Load(file.Provider(cleaned), yaml.Parser()); err != nil {
				return fmt.Errorf("loading config file %s: %w", cleaned, err)
			}
			return nil
		}
	}
	return nil
}

// Top-level koanf keys, derived from the Config struct's tags so a new
// multi-word top-level field can never silently miss the env mapping the way
// SECRET_BACKEND did under first-underscore splitting (#710). Scalar keys map
// only on an exact env-name match; nested keys (struct/map fields) also map
// PREFIX_rest -> prefix.rest (DB_URL -> db.url).
var envTopLevelScalarKeys, envTopLevelNestedKeys = topLevelKoanfKeys()

func topLevelKoanfKeys() (scalar, nested map[string]bool) {
	scalar, nested = map[string]bool{}, map[string]bool{"auth": true}
	t := reflect.TypeOf(billing.Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("koanf"), ",")
		if name == "" || name == "-" {
			continue
		}
		ft := t.Field(i).Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct || ft.Kind() == reflect.Map {
			nested[name] = true
		} else {
			scalar[name] = true
		}
	}
	return scalar, nested
}

// envKeyToConfigKey maps an env var name to its koanf config key.
// Examples: SECRET_BACKEND -> secret_backend, DB_URL -> db.url. Names that
// route to no config section return "" and are never loaded (the process env
// is full of vars that are not ours — PATH, HOME, compose interpolation vars);
// or#915 pairs this with a strict ErrorUnused unmarshal, so a name INSIDE one
// of our sections that hits no field (DB_TYPO) refuses boot instead of
// silently dropping.
func envKeyToConfigKey(s string) string {
	s = strings.ToLower(s)

	// VAULT_ADDR is HashiCorp's canonical name for the server URL; the field is
	// vault.address (the mechanical split would yield vault.addr).
	if s == "vault_addr" {
		return "vault.address"
	}

	// AUTHKIT_ACTIVE_KEY_ID / AUTHKIT_ACTIVE_PRIVATE_KEY_PEM /
	// AUTHKIT_PUBLIC_KEYS are AuthKit's canonical inline-key env names
	// (mirrored by cmd/authkit-server; ak#266/v0.89.0 prefixed them, or#917
	// follows); the mechanical split would look for a nonexistent "authkit"
	// top-level prefix, so these are dead without the special case. The
	// unprefixed names are poison — Load refuses boot on them.
	switch s {
	case "auth_direct_peer_ip":
		return "auth.direct_peer_ip"
	case "auth_naming_enabled":
		return "auth.naming.enabled"
	case "auth_naming_rename_interval":
		return "auth.naming.rename_interval"
	case "auth_naming_former_names_mode":
		return "auth.naming.former_names.mode"
	case "auth_naming_former_names_duration":
		return "auth.naming.former_names.duration"
	case "authkit_active_key_id":
		return "auth.active_key_id"
	case "authkit_active_private_key_pem":
		return "auth.active_private_key_pem"
	case "authkit_public_keys":
		return "auth.public_keys"
	case "authkit_keys_path":
		return "auth.keys_path"
	}

	if envTopLevelScalarKeys[s] || envTopLevelNestedKeys[s] {
		return s
	}
	for prefix := range envTopLevelNestedKeys {
		if rest, ok := strings.CutPrefix(s, prefix+"_"); ok && rest != "" {
			return prefix + "." + rest
		}
	}
	return ""
}

// LoadOption customizes Load. WithOverride is the CLI-flag path (or#915):
// flags ride the same koanf pipeline as everything else via a confmap overlay
// loaded ABOVE env (flag beats env beats yaml) — never by writing into the
// process environment behind the loader's back.
type LoadOption func(*loadOptions)

type loadOptions struct {
	overrides map[string]any
}

// WithOverride overlays one koanf config key (e.g. "test_mode",
// "provider_write_mode") above every other source.
func WithOverride(key string, value any) LoadOption {
	return func(o *loadOptions) {
		if o.overrides == nil {
			o.overrides = map[string]any{}
		}
		o.overrides[key] = value
	}
}

func Load(configPath string, opts ...LoadOption) (*Config, error) {
	return load(configPath, false, opts...)
}

// LoadDatabase loads and validates only database configuration for offline
// operator commands. It preserves the normal file, mounted-secret, environment
// and flag precedence, without requiring provider or authentication credentials.
// The returned config is unsuitable for constructing a server runtime.
func LoadDatabase(configPath string, opts ...LoadOption) (*Config, error) {
	return load(configPath, true, opts...)
}

func load(configPath string, databaseOnly bool, opts ...LoadOption) (*Config, error) {
	var options loadOptions
	for _, opt := range opts {
		opt(&options)
	}
	k := koanf.New(".")

	// Start from sensible defaults so zero-config works in containers/compose —
	// except the environment. GetDefaultBillingConfig is the DEVELOPMENT default
	// set; Load must not inherit that posture, so ENV is cleared here and
	// required below once every source has been overlaid (SEC-18).
	cfg := &Config{Config: billing.GetDefaultBillingConfig(), Auth: &AuthConfig{}}
	cfg.Env = ""

	// A .env in the working directory is a real config source, so consuming
	// one is LOGGED (or#915): a deployment silently absorbing a stray .env is
	// the quiet failure mode the env audit flagged.
	if err := godotenv.Load(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, err
		}
	} else if abs, absErr := filepath.Abs(".env"); absErr == nil {
		log.Infof("config: loaded environment overrides from %s", abs)
	}

	if configPath == "" {
		if envPath := strings.TrimSpace(os.Getenv("OPENRAILS_CONFIG")); envPath != "" {
			configPath = envPath
		} else if envPath := strings.TrimSpace(os.Getenv("BILLING_CONFIG")); envPath != "" {
			configPath = envPath
		} else {
			configPath = "config.yaml"
		}
	}

	if err := loadConfigIfExists(k, configPath); err != nil {
		return nil, err
	}

	envCallbackWithValue := func(key string, value string) (string, interface{}) {
		upperKey := strings.ToUpper(key)
		if upperKey == "MERCHANT" || upperKey == "AUTH_ISSUERS" || upperKey == "CORS_ORIGINS" || strings.HasPrefix(upperKey, "RAILS_") || strings.HasPrefix(upperKey, "STORE_") {
			return "", nil
		}
		// DB_ADMIN_PASSWORD belongs to the privileged migration role and is a
		// docker-compose interpolation var, never server config.
		// VAULT_SECRETS_PATH is consumed directly by config.SecretFiles, not a
		// vault.* field. Without these skips the strict unmarshal would refuse
		// boot on db.admin_password / vault.secrets_path.
		if upperKey == "DB_ADMIN_PASSWORD" || upperKey == "VAULT_SECRETS_PATH" {
			return "", nil
		}

		mapped := envKeyToConfigKey(key)
		if mapped == "" {
			return "", nil
		}

		v := strings.TrimSpace(value)

		// auth.public_keys is a JSON STRING field ({kid: PEM}, parsed by the
		// control plane at construction), not structured config — decoding it
		// here would hand the strict unmarshal a map for a string field and
		// refuse boot (or#917).
		if mapped == "auth.public_keys" {
			return mapped, v
		}

		// Allow JSON for arrays/objects where the remaining infrastructure config
		// uses structured values.
		if len(v) >= 2 {
			if (v[0] == '[' && v[len(v)-1] == ']') || (v[0] == '{' && v[len(v)-1] == '}') {
				var decoded interface{}
				if err := json.Unmarshal([]byte(v), &decoded); err == nil {
					return mapped, decoded
				}
			}
		}

		return mapped, v
	}

	// Operator-mounted secret files (filename = env-var name) load BELOW env,
	// so env wins. This is the default non-SaaS secret path: Vault renders
	// files into the mounted dir; no live Vault connection needed.
	secretFiles, err := billing.SecretFiles()
	if err != nil {
		return nil, err
	}
	if len(secretFiles) > 0 {
		vals := map[string]interface{}{}
		for name, value := range secretFiles {
			if key, v := envCallbackWithValue(name, value); key != "" {
				vals[key] = v
			}
		}
		if err := k.Load(confmap.Provider(vals, "."), nil); err != nil {
			return nil, fmt.Errorf("loading mounted secret files: %w", err)
		}
	}

	if err := k.Load(env.ProviderWithValue("", ".", envCallbackWithValue), nil); err != nil {
		return nil, fmt.Errorf("loading environment variables: %w", err)
	}

	// CLI-flag overrides load ABOVE env (or#915): flag beats env beats yaml,
	// without any os.Setenv back-door.
	if len(options.overrides) > 0 {
		if err := k.Load(confmap.Provider(options.overrides, "."), nil); err != nil {
			return nil, fmt.Errorf("loading flag overrides: %w", err)
		}
	}

	// Merchant configuration authority is broader than provider secrets. Retired
	// spellings must fail even if a new spelling is also supplied.
	if _, present := os.LookupEnv("CATALOG_SOURCE"); k.Exists("catalog_source") || present {
		return nil, fmt.Errorf("catalog_source / CATALOG_SOURCE was removed: catalogs always use the database; set allow_catalog_updates / ALLOW_CATALOG_UPDATES to enable ordinary catalog mutations")
	}
	if _, present := os.LookupEnv("MERCHANT_SOURCE"); k.Exists("merchant_source") || present {
		return nil, fmt.Errorf("merchant_source / MERCHANT_SOURCE was renamed: use merchant_config_source / MERCHANT_CONFIG_SOURCE (manifest|api)")
	}

	if databaseOnly {
		dbConfig := cfg.DB
		if err := k.UnmarshalWithConf("db", dbConfig, koanf.UnmarshalConf{
			Tag: "koanf",
			DecoderConfig: &mapstructure.DecoderConfig{
				DecodeHook: mapstructure.ComposeDecodeHookFunc(mapstructure.StringToTimeDurationHookFunc(), mapstructure.TextUnmarshallerHookFunc()),
				Result:     dbConfig, WeaklyTypedInput: true, ErrorUnused: true,
			},
		}); err != nil {
			return nil, fmt.Errorf("unmarshaling database config: %w", err)
		}
		databaseConfig := &Config{Config: &billing.Config{DB: dbConfig}}
		databaseConfig.DB.URL = databaseConfig.DB.GetConnectionString()
		databaseConfig.DB.Schema = databaseConfig.DB.SchemaName()
		if err := billing.ValidateDatabase(databaseConfig.DB); err != nil {
			return nil, err
		}
		return databaseConfig, nil
	}

	// HARD CUT (#469): the AuthKit control plane is always on in standalone
	// mode; the verifier-only deployment mode is gone. The former
	// auth.control_plane.enabled knob is rejected — not silently ignored — so a
	// deployment that believed it was toggling the control plane finds out at
	// load time. Private posture is fixed in this repo, not a disabled control
	// plane.
	if k.Exists("auth.control_plane.enabled") {
		return nil, fmt.Errorf("auth.control_plane.enabled was removed (#469): the control plane is always on in standalone mode — delete the key; private/self-hosted registration is the default")
	}
	if k.Exists("merchant_cors") {
		return nil, fmt.Errorf("merchant_cors was removed (#519): browser CORS belongs to the host app, not OpenRails merchant config")
	}
	// HARD CUT (#710): the deprecated provider_write_mode alias is gone.
	if k.Exists("mode") || os.Getenv("MODE") != "" || os.Getenv("BILLING_MODE") != "" {
		return nil, fmt.Errorf("mode was removed (#710): set provider_write_mode (env PROVIDER_WRITE_MODE, flag --provider-write-mode) to full|limited|readonly")
	}
	// HARD CUT (or#915): TEST_ENV was documented in .env.example but never
	// consumed — an operator who set it believed they had chosen a credential
	// posture and had not. Refuse, never silently drop.
	if os.Getenv("TEST_ENV") != "" {
		return nil, fmt.Errorf("TEST_ENV was never consumed and is removed (or#915): set test_mode (env TEST_MODE, flag --test-mode) to sandbox|live")
	}
	// #712: ad-hoc library env knobs moved into config; the old names fail loudly.
	if os.Getenv("OPENRAILS_CATALOG_RECONCILIATION_INTERVAL") != "" {
		return nil, fmt.Errorf("OPENRAILS_CATALOG_RECONCILIATION_INTERVAL was renamed (#712): set catalog_reconciliation_interval (env CATALOG_RECONCILIATION_INTERVAL)")
	}
	if os.Getenv("OPENRAILS_SQL_TRACE") != "" {
		return nil, fmt.Errorf("OPENRAILS_SQL_TRACE was renamed (#712): set db.sql_trace (env DB_SQL_TRACE)")
	}
	// HARD CUT (or#917, authkit ak#266/v0.89.0): the inline signing-key env
	// names are AUTHKIT_-prefixed, matching the authkit binary. The old names
	// would silently boot an ephemeral dev key / keys.json fallback instead of
	// the operator's key — refuse, never silently drop key material.
	for _, old := range []string{"ACTIVE_KEY_ID", "ACTIVE_PRIVATE_KEY_PEM", "PUBLIC_KEYS"} {
		if os.Getenv(old) != "" {
			return nil, fmt.Errorf("%s was renamed (or#917, authkit ak#266): set AUTHKIT_%s", old, old)
		}
	}
	if k.Exists("billing_hot_path") || os.Getenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY") != "" || os.Getenv("BILLING_HOT_PATH_FAIL_POLICY") != "" {
		return nil, fmt.Errorf("billing_hot_path was removed: OpenRails does not enforce client degraded-mode policy; configure any fail-open/fail-closed behavior in the calling client")
	}
	// HARD CUT (#735): ClickHouse is gone — metrics and dunning forensics are
	// Postgres-backed. Stale config fails loudly, never silently ignored.
	if k.Exists("clickhouse") || hasEnvPrefix("CLICKHOUSE_") || hasEnvPrefix("BILLING_CLICKHOUSE_") {
		return nil, fmt.Errorf("clickhouse config was removed (#735): delete the clickhouse yaml key and CLICKHOUSE_* env vars; analytics/forensics read Postgres")
	}

	// or#893 phase 3: the retired families below used to warn-and-boot. An
	// operator who believed ignored security or tenant config was active had no
	// way to find out. Every one of them now REFUSES boot with the error that
	// names its replacement — no aliases, no dual reads.
	retiredPrivatePort := k.Exists("private_port") || os.Getenv("PRIVATE_PORT") != ""
	retiredStoreConfig := k.Exists("store") || hasEnvPrefix("STORE_")
	retiredMerchantConfig := k.Exists("merchant") || os.Getenv("MERCHANT") != ""
	retiredCORSConfig := k.Exists("cors_origins") || os.Getenv("CORS_ORIGINS") != ""
	retiredDBRequireRLS := k.Exists("db.require_rls") || os.Getenv("DB_REQUIRE_RLS") != ""
	retiredAuthIssuers := k.Exists("auth.issuers") || k.Exists("auth.expected_audience") ||
		os.Getenv("AUTH_ISSUERS") != "" || os.Getenv("AUTH_EXPECTED_AUDIENCE") != ""
	retiredRails := k.Exists("rails") || hasEnvPrefix("RAILS_")
	retiredControlPlaneLegacy := k.Exists("auth.control_plane.issuer") ||
		k.Exists("auth.control_plane.issued_audiences") ||
		k.Exists("auth.control_plane.expected_audiences") ||
		k.Exists("auth.control_plane.public_user_registration") ||
		k.Exists("auth.control_plane.public_tenant_registration") ||
		k.Exists("auth.control_plane.public_hosted") ||
		k.Exists("auth.control_plane.token_prefix") ||
		k.Exists("auth.control_plane.platform_admin_user_id") ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUER") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_EXPECTED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_USER_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_TENANT_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_HOSTED") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_TOKEN_PREFIX") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_BOOTSTRAP_ADMIN_SERVICE_TOKEN_NAME") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PLATFORM_ADMIN_USER_ID") != ""
	if retiredStoreConfig {
		return nil, fmt.Errorf("store config was removed (#520): seed merchant profile fields with openrails push-merchant-config under merchants[].profile; delete the store yaml key and STORE_* env vars")
	}
	if retiredMerchantConfig {
		return nil, fmt.Errorf("merchant config was removed (#520/#521): seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant — delete the merchant yaml key and MERCHANT env var")
	}
	if retiredCORSConfig {
		return nil, fmt.Errorf("cors_origins config was removed (#519/#765): browser CORS is a fixed engine policy (checkout/self-service = public *, everything else = none) — bearer JWTs are the security boundary, not a configurable origin allowlist; delete the cors_origins yaml key and CORS_ORIGINS env var")
	}
	if retiredDBRequireRLS {
		return nil, fmt.Errorf("db.require_rls config was removed: merchant isolation uses explicit scoped queries, not PostgreSQL RLS; delete the db.require_rls yaml key and DB_REQUIRE_RLS env var")
	}
	if retiredAuthIssuers {
		return nil, fmt.Errorf("auth.issuers / auth.expected_audience config was removed (#521/#527): declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest; delete the keys and AUTH_ISSUERS / AUTH_EXPECTED_AUDIENCE env vars")
	}
	if retiredRails {
		return nil, fmt.Errorf("rails config was removed (#521): seed merchant PSPs and secrets with openrails push-merchant-config under merchants[].psps; delete the rails yaml key and RAILS_* env vars")
	}
	if retiredControlPlaneLegacy {
		return nil, fmt.Errorf("auth.control_plane config was removed (#521): use auth.issuer (env AUTH_ISSUER) — audiences are fixed to openrails, standalone public hosted registration is unavailable in this repo, and platform-superadmin belongs in openrails-saas; delete the auth.control_plane keys and AUTH_CONTROL_PLANE_* env vars")
	}
	if retiredPrivatePort {
		return nil, fmt.Errorf("private_port was removed: OpenRails serves a single HTTP listener and there is no separate internal port; delete the private_port yaml key and PRIVATE_PORT env var")
	}

	// Unmarshal into config struct (overlay onto defaults). Strict (or#915,
	// matching the merchant overlay's ErrorUnused): a yaml key or an env var
	// inside one of our sections that hits no struct field refuses boot —
	// "config I set is silently ignored" is the failure mode this kills.
	// (envKeyToConfigKey already drops env names outside our sections, so the
	// ambient process environment cannot trip this.)
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
				mapstructure.TextUnmarshallerHookFunc(),
			),
			Result:           cfg,
			WeaklyTypedInput: true,
			ErrorUnused:      true,
		},
	}); err != nil {
		return nil, fmt.Errorf("unmarshaling config (unknown keys refuse boot — or#915): %w", err)
	}

	// SEC-18: ENV is REQUIRED and has no default. Every other knob can fail
	// closed on its own; this one decides WHICH way the others fail, so it must
	// be declared, not inferred. Silently reading unset as "development" meant a
	// container deployed without ENV kept merchant secrets in PLAINTEXT.
	cfg.Env = strings.TrimSpace(cfg.Env)
	if cfg.Env == "" {
		return nil, fmt.Errorf("ENV is required (SEC-18): set env (env ENV) to development for a local/dev deployment, or to production/staging/<name> — there is no default, because the permissive posture (plaintext merchant secrets) is the development one")
	}

	// Sandbox-by-default in development (#355/#745): when test_mode is not
	// explicitly provided, a dev boot defaults to sandbox credentials so a
	// local run can never accidentally move real money against live rail
	// credentials — the dangerous case is silent ("forgot to set it"), so the
	// safe value is the one you get by omission. To run live locally, set
	// test_mode=live explicitly (env TEST_MODE=live, flag --test-mode=live).
	//
	// Outside development test_mode is REQUIRED (or#915, fail-closed like
	// provider_write_mode): the old silent live default meant an operator who
	// believed an ignored TEST_ENV had put them in test was actually pointed
	// at live credentials. The credential posture decides whether real money
	// moves — it must be declared, never inherited by omission. This only
	// governs standalone Load(); embedded hosts build the Config
	// programmatically and MUST supply their own value — embedded.New refuses
	// to construct otherwise.
	if !k.Exists("test_mode") {
		if !cfg.IsDev() {
			return nil, fmt.Errorf("test_mode is required outside development (or#915): set test_mode (env TEST_MODE, flag --test-mode) to live or sandbox — the credential posture must be declared explicitly, there is no default")
		}
		cfg.TestMode = billing.CredentialPostureSandbox
	}

	// The control plane is mandatory in standalone mode (#469). In development
	// auth.issuer defaults to the deployment's own base URL so zero-config dev
	// boots; outside development a missing issuer fails fast at control-plane
	// construction.
	if cfg.Auth == nil {
		cfg.Auth = &AuthConfig{}
	}
	if strings.TrimSpace(cfg.Auth.Issuer) == "" {
		if cfg.IsDev() {
			issuer := strings.TrimSpace(cfg.APIURL)
			if issuer == "" {
				port := int(cfg.Port)
				if port == 0 {
					port = 3053
				}
				issuer = fmt.Sprintf("http://localhost:%d", port)
			}
			cfg.Auth.Issuer = strings.TrimRight(issuer, "/")
		}
	}

	// Assemble DB URL from pieces if not explicitly set
	if cfg.DB != nil {
		cfg.DB.URL = cfg.DB.GetConnectionString()
	}

	// Normalize the OpenRails Postgres schema to its canonical form (#165) so the
	// stored config value matches what SchemaName() resolves to. Validation of the
	// identifier happens in Validate(). Defaults to `billing` (config.DefaultSchema).
	if cfg.DB != nil {
		cfg.DB.Schema = cfg.DB.SchemaName()
	}

	// Validate the loaded configuration
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

func hasEnvPrefix(prefix string) bool {
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
