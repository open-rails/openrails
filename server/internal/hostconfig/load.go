package hostconfig

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server"
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

// Top-level keys of both decode targets, from their yaml tags, so a new
// multi-word top-level key can never miss the environment mapping the way
// PUBLIC_BILLING_BASE_URL would under first-underscore splitting. Scalar keys
// map only on an exact name; a section also maps PREFIX_rest to prefix.rest
// (DATABASE_URL to database.url).
var envTopLevelScalarKeys, envTopLevelNestedKeys = topLevelKeys(reflect.TypeFor[server.Config](), reflect.TypeFor[Config]())

func topLevelKeys(types ...reflect.Type) (scalar, nested map[string]bool) {
	scalar, nested = map[string]bool{}, map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			name, opts, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			ft := t.Field(i).Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch {
			case opts == "inline":
				walk(ft)
			case name == "" || name == "-":
			case ft.Kind() == reflect.Struct || ft.Kind() == reflect.Map:
				nested[name] = true
			default:
				scalar[name] = true
			}
		}
	}
	for _, t := range types {
		walk(t)
	}
	return scalar, nested
}

// envNames are the variables whose key is not their mechanical spelling:
// HashiCorp's VAULT_ADDR and VAULT_CACERT, AuthKit's AUTHKIT_* inline key (the
// unprefixed names refuse boot), and naming's nested former_names.
var envNames = map[string]string{
	"vault_addr":                     "vault.address",
	"vault_cacert":                   "vault.ca_cert",
	"naming_former_names_mode":       "naming.former_names.mode",
	"naming_former_names_duration":   "naming.former_names.duration",
	"authkit_active_key_id":          "signing_key.active_key_id",
	"authkit_active_private_key_pem": "signing_key.active_private_key_pem",
	"authkit_public_keys":            "signing_key.public_keys",
}

// fileOnlySections are policy, set in config.yaml alone: the environment
// holds secrets and per-environment facts. A variable naming one refuses
// boot.
var fileOnlySections = []string{"rate_limits", "user_action_rate_limit", "destructive_action_limits", "auth.http.rate_limits"}

// fileOnlySection is the file-only section key is in, "" for none.
func fileOnlySection(key string) string {
	for _, section := range fileOnlySections {
		if key == section || strings.HasPrefix(key, section+".") {
			return section
		}
	}
	return ""
}

// envKeyToConfigKey maps an environment variable's name to its key:
// PUBLIC_BILLING_BASE_URL is public_billing_base_url, DATABASE_URL
// database.url. A name that routes to no section is "" and never loaded (the
// process environment is full of names that are not ours: PATH, HOME,
// Compose's interpolation); a name inside a section that hits no key
// (DATABASE_TYPO) refuses boot like any unknown key.
func envKeyToConfigKey(s string) string {
	s = strings.ToLower(s)

	if key, ok := envNames[s]; ok {
		return key
	}
	// AUTH_* is AuthKit's configuration, by AuthKit's keys.
	if rest, ok := strings.CutPrefix(s, "auth_"); ok && rest != "" {
		return "auth." + authPath(rest)
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

// Load reads the binary's configuration: the file at configPath
// (OPENRAILS_CONFIG, else config.yaml), mounted secret files, then the
// environment, each over the last, onto the defaults. A key that neither
// the server nor the binary reads refuses the load.
func Load(configPath string) (*Config, error) {
	k := koanf.New(".")
	cfg := defaults()

	// A .env in the working directory is a real config source, so consuming
	// one is reported (Config.EnvFile): a deployment silently absorbing a
	// stray .env is the quiet failure mode the env audit flagged.
	if err := godotenv.Load(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, err
		}
	} else if abs, absErr := filepath.Abs(".env"); absErr == nil {
		cfg.EnvFile = abs
	}

	if configPath == "" {
		configPath = cmp.Or(strings.TrimSpace(os.Getenv("OPENRAILS_CONFIG")), "config.yaml")
	}
	if err := loadConfigIfExists(k, configPath); err != nil {
		return nil, err
	}

	var fileOnly []string
	envCallbackWithValue := func(key string, value string) (string, interface{}) {
		upperKey := strings.ToUpper(key)
		if serviceLink(upperKey, strings.TrimSpace(value)) {
			return "", nil
		}
		if upperKey == "MERCHANT" || upperKey == "AUTH_ISSUERS" || upperKey == "CORS_ORIGINS" || strings.HasPrefix(upperKey, "RAILS_") || strings.HasPrefix(upperKey, "STORE_") {
			return "", nil
		}
		// VAULT_SECRETS_PATH names the mounted secret files directory, which
		// the loader reads itself; mapping it to vault.secrets_path would
		// declare a Vault connection on its own.
		if upperKey == "VAULT_SECRETS_PATH" {
			return "", nil
		}

		mapped := envKeyToConfigKey(key)
		if mapped == "" {
			return "", nil
		}
		if section := fileOnlySection(mapped); section != "" && !slices.ContainsFunc(retiredSettings, func(r retiredSetting) bool { return r.env == upperKey }) {
			fileOnly = append(fileOnly, fmt.Sprintf("%s is set in config.yaml, not the environment: unset %s", section, upperKey))
			return "", nil
		}

		v := strings.TrimSpace(value)

		// signing_key.public_keys is a JSON STRING field ({kid: PEM}),
		// not structured config: decoding it here would hand the strict
		// unmarshal a map for a string field and refuse boot.
		if mapped == "signing_key.public_keys" {
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
	// so env wins. This is the default self-hosted secret path: Vault renders
	// files into the mounted dir; no live Vault connection needed.
	secretsDir := cmp.Or(strings.TrimSpace(os.Getenv("VAULT_SECRETS_PATH")), config.DefaultSecretsPath)
	secretFiles, err := config.SecretFiles(secretsDir)
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
	if len(fileOnly) > 0 {
		slices.Sort(fileOnly)
		return nil, errors.New(strings.Join(fileOnly, "; "))
	}

	// Merchant configuration authority is broader than provider secrets. Retired
	// spellings must fail even if a new spelling is also supplied.
	if _, present := os.LookupEnv("CATALOG_SOURCE"); k.Exists("catalog_source") || present {
		return nil, fmt.Errorf("catalog_source / CATALOG_SOURCE was removed: catalogs always use the database; documents and edits both change it")
	}
	if _, present := os.LookupEnv("VAULT_ENABLED"); k.Exists("vault.enabled") || present {
		return nil, fmt.Errorf("vault.enabled / VAULT_ENABLED was removed: declaring vault (or a VAULT_* setting) connects to it")
	}
	if _, present := os.LookupEnv("CATALOG_EDITS"); k.Exists("catalog_edits") || present {
		return nil, fmt.Errorf("catalog_edits / CATALOG_EDITS was removed: catalog edits over HTTP are always on; a document skips what an edit set")
	}
	if _, present := os.LookupEnv("MERCHANT_SOURCE"); k.Exists("merchant_source") || present {
		return nil, fmt.Errorf("merchant_source / MERCHANT_SOURCE was removed: name vault.kv_mount and Vault holds merchant configuration")
	}
	if _, present := os.LookupEnv("MERCHANT_CONFIG_SOURCE"); k.Exists("merchant_config_source") || present {
		return nil, fmt.Errorf("merchant_config_source / MERCHANT_CONFIG_SOURCE was removed: name vault.kv_mount and Vault holds merchant configuration")
	}
	for key, env := range map[string]string{
		"secret_backend": "SECRET_BACKEND", "credential_snapshot_id": "CREDENTIAL_SNAPSHOT_ID",
		"credential_read_only": "CREDENTIAL_READ_ONLY", "alert_secret_backend": "ALERT_SECRET_BACKEND",
	} {
		if _, present := os.LookupEnv(env); k.Exists(key) || present {
			return nil, fmt.Errorf("%s / %s was removed: name vault.kv_mount and Vault holds merchant configuration; without it the merchant manifest is the configuration", key, env)
		}
	}
	if _, present := os.LookupEnv("ENCRYPTION_MASTER_KEY"); k.Exists("encryption") || present {
		return nil, fmt.Errorf("encryption / ENCRYPTION_MASTER_KEY was removed: OpenRails keeps no credentials in the database")
	}
	// Settings that left the file: an environment variable or a mounted
	// secret file names one as surely as a key does.
	for _, retired := range retiredSettings {
		inSecrets := false
		for name := range secretFiles {
			inSecrets = inSecrets || retired.names(name)
		}
		if k.Exists(retired.key) || retired.inEnv() || inSecrets {
			return nil, fmt.Errorf("%s / %s was removed: %s", retired.key, retired.env, retired.now)
		}
	}

	for _, retired := range []string{"ENV", "API_URL", "NEW_SUBSCRIPTION_COLLECTION_POLICY"} {
		_, present := os.LookupEnv(retired)
		if present || k.Exists(strings.ToLower(retired)) {
			return nil, fmt.Errorf("%s is retired: use explicit security settings, public_billing_base_url and default engine collection", retired)
		}
	}

	for _, old := range []string{"admin_console.enabled", "admin_console.path"} {
		if k.Exists(old) {
			return nil, fmt.Errorf("%s was removed: use admin_console.disabled to omit the console at /admin; configure admin_console.issuers and local_sign_in for sign-in", old)
		}
	}
	if k.Exists("admin_console.issuer") {
		return nil, fmt.Errorf("admin_console.issuer was renamed: declare admin_console.issuers keyed by merchant id or name (root for operators)")
	}
	// HARD CUT (#469): the AuthKit control plane is always on in standalone
	// mode; the verifier-only deployment mode is gone. The former
	// auth.control_plane.enabled knob is rejected — not silently ignored — so a
	// deployment that believed it was toggling the control plane finds out at
	// load time. Private posture is fixed in this repo, not a disabled control
	// plane.
	if k.Exists("auth.control_plane.enabled") {
		return nil, fmt.Errorf("auth.control_plane.enabled was removed: the control plane is always on in standalone mode — delete the key; private/self-hosted registration is the default")
	}
	if k.Exists("dashboard_base_url") {
		return nil, fmt.Errorf("dashboard_base_url was removed: alert links point at the server's console")
	}
	if k.Exists("ccbill_webhook_ip_allowlist") {
		return nil, fmt.Errorf("ccbill_webhook_ip_allowlist was removed: CCBill webhooks are accepted from CCBill's published ranges")
	}
	if k.Exists("merchant_cors") {
		return nil, fmt.Errorf("merchant_cors was removed: browser CORS belongs to the host app, not OpenRails merchant config")
	}
	if k.Exists("mode") || os.Getenv("MODE") != "" || os.Getenv("BILLING_MODE") != "" {
		return nil, fmt.Errorf("mode was removed: %s", WritePostureHint)
	}
	// #712: ad-hoc library env knobs moved into config; the old names fail loudly.
	if os.Getenv("OPENRAILS_SQL_TRACE") != "" {
		return nil, fmt.Errorf("OPENRAILS_SQL_TRACE was renamed: set database.sql_trace (env DATABASE_SQL_TRACE)")
	}
	// HARD CUT (or#917, authkit ak#266/v0.89.0): the inline signing-key env
	// names are AUTHKIT_-prefixed, matching the authkit binary. The old names
	// would silently boot an ephemeral dev key / keys.json fallback instead of
	// the operator's key — refuse, never silently drop key material.
	for _, old := range []string{"ACTIVE_KEY_ID", "ACTIVE_PRIVATE_KEY_PEM", "PUBLIC_KEYS"} {
		if os.Getenv(old) != "" {
			return nil, fmt.Errorf("%s was renamed: set AUTHKIT_%s", old, old)
		}
	}
	if k.Exists("billing_hot_path") || os.Getenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY") != "" || os.Getenv("BILLING_HOT_PATH_FAIL_POLICY") != "" {
		return nil, fmt.Errorf("billing_hot_path was removed: OpenRails does not enforce client degraded-mode policy; configure any fail-open/fail-closed behavior in the calling client")
	}
	_, oldCardVaultEnv := os.LookupEnv("PLATFORM_VAULT")
	retiredCardVault := k.Exists("platform_vault") || oldCardVaultEnv || hasEnvPrefix("PLATFORM_VAULT_")
	for name := range secretFiles {
		retiredCardVault = retiredCardVault || name == "PLATFORM_VAULT" || strings.HasPrefix(name, "PLATFORM_VAULT_")
	}
	if retiredCardVault {
		return nil, fmt.Errorf("platform_vault was renamed to card_vault: use CARD_VAULT_* env vars and secret filenames; keep the existing credential document path")
	}

	// HARD CUT (#1176): email goes through any SMTP server.
	retiredSendGrid := k.Exists("sendgrid") || hasEnvPrefix("SENDGRID_")
	for name := range secretFiles {
		retiredSendGrid = retiredSendGrid || strings.HasPrefix(name, "SENDGRID_")
	}
	if retiredSendGrid {
		return nil, fmt.Errorf("sendgrid config was removed: email goes through any SMTP server; set email_smtp (EMAIL_SMTP_HOST, EMAIL_SMTP_PORT, EMAIL_SMTP_USERNAME, EMAIL_SMTP_PASSWORD, EMAIL_SMTP_FROM): SendGrid is host smtp.sendgrid.net, port 587, username apikey, password the API key; delete the sendgrid yaml key and SENDGRID_* env vars and secret files")
	}

	// or#893 phase 3: the retired families below used to warn-and-boot. An
	// operator who believed ignored security or tenant config was active had no
	// way to find out. Every one of them now REFUSES boot with the error that
	// names its replacement — no aliases, no dual reads.
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
	// HARD CUT (#1185): auth is AuthKit's own configuration, by AuthKit's
	// keys; the server's former auth keys name their replacements.
	if _, present := os.LookupEnv("AUTHKIT_KEYS_PATH"); present {
		return nil, fmt.Errorf("AUTHKIT_KEYS_PATH was renamed: set AUTH_KEYS_PATH (auth.keys.path)")
	}
	for old, now := range retiredAuthKeys {
		if k.Exists(old) {
			return nil, fmt.Errorf("%s was removed: auth is AuthKit's configuration; set %s", old, now)
		}
	}
	if retiredStoreConfig {
		return nil, fmt.Errorf("store config was removed: seed merchant profile fields with openrails push-merchant-config under merchants[].profile; delete the store yaml key and STORE_* env vars")
	}
	if retiredMerchantConfig {
		return nil, fmt.Errorf("merchant config was removed: seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant — delete the merchant yaml key and MERCHANT env var")
	}
	if retiredCORSConfig {
		return nil, fmt.Errorf("cors_origins config was removed: browser CORS is a fixed engine policy (purchase/self-service = public *, everything else = none) — bearer JWTs are the security boundary, not a configurable origin allowlist; delete the cors_origins yaml key and CORS_ORIGINS env var")
	}
	if retiredDBRequireRLS {
		return nil, fmt.Errorf("db.require_rls config was removed: merchant isolation uses explicit scoped queries, not PostgreSQL RLS; delete the db.require_rls yaml key and DB_REQUIRE_RLS env var")
	}
	if retiredAuthIssuers {
		return nil, fmt.Errorf("auth.issuers / auth.expected_audience config was removed: declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest; delete the keys and AUTH_ISSUERS / AUTH_EXPECTED_AUDIENCE env vars")
	}
	if retiredRails {
		return nil, fmt.Errorf("rails config was removed: seed merchant PSPs and secrets with openrails push-merchant-config under merchants[].psps; delete the rails yaml key and RAILS_* env vars")
	}
	if retiredControlPlaneLegacy {
		return nil, fmt.Errorf("auth.control_plane config was removed: use auth.token.issuer (env AUTH_TOKEN_ISSUER) — audiences are fixed to openrails, standalone public hosted registration is unavailable in this repo, and platform-superadmin belongs in the hosted product; delete the auth.control_plane keys and AUTH_CONTROL_PLANE_* env vars")
	}

	// The db section merged into database.
	retiredDB := k.Exists("db")
	for _, name := range retiredDBEnv {
		_, inEnv := os.LookupEnv(name)
		_, inSecrets := secretFiles[name]
		retiredDB = retiredDB || inEnv || inSecrets
	}
	if retiredDB {
		return nil, fmt.Errorf("db was merged into database: set database.url (DATABASE_URL), or database.host, port, name, username and password (DATABASE_HOST, …, DATABASE_NAME for DB_DATABASE); sslmode, sslrootcert, sql_trace and lease_conns moved beside them")
	}

	// One tree, two targets: a key either reads is known, and one neither
	// reads refuses boot ("config I set is silently ignored" is the failure
	// this kills). envKeyToConfigKey already drops names outside our
	// sections, so the ambient environment cannot trip it.
	var unused [][]string
	for _, target := range []any{&cfg.Server, cfg} {
		var meta mapstructure.Metadata
		if err := k.UnmarshalWithConf("", target, koanf.UnmarshalConf{
			Tag: "yaml",
			DecoderConfig: &mapstructure.DecoderConfig{
				DecodeHook: mapstructure.ComposeDecodeHookFunc(
					mapstructure.StringToTimeDurationHookFunc(),
					mapstructure.StringToSliceHookFunc(","),
					mapstructure.TextUnmarshallerHookFunc(),
				),
				Result: target, WeaklyTypedInput: true, Metadata: &meta,
				SquashTagOption: "inline", MatchName: func(key, field string) bool { return key == field },
			},
		}); err != nil {
			return nil, fmt.Errorf("unmarshaling config: %w", err)
		}
		unused = append(unused, meta.Unused)
	}
	if unknown := unknownKeys(unused...); len(unknown) > 0 {
		return nil, fmt.Errorf("unknown configuration keys refuse boot: %s", strings.Join(unknown, ", "))
	}
	if err := cfg.finish(secretsDir); err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return cfg, nil
}

// unknownKeys are the keys every decode left unused: a key, or one of its
// ancestors, in each list.
func unknownKeys(unused ...[]string) []string {
	unusedIn := func(list []string, key string) bool {
		for _, u := range list {
			if key == u || strings.HasPrefix(key, u+".") {
				return true
			}
		}
		return false
	}
	var out []string
	for _, list := range unused {
		for _, key := range list {
			all := true
			for _, other := range unused {
				all = all && unusedIn(other, key)
			}
			if all && !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	// The deepest key of each unknown subtree says the most.
	slices.Sort(out)
	return slices.DeleteFunc(slices.Clone(out), func(key string) bool {
		return slices.ContainsFunc(out, func(other string) bool { return strings.HasPrefix(other, key+".") })
	})
}

func hasEnvPrefix(prefix string) bool {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, prefix) && !serviceLink(name, value) {
			return true
		}
	}
	return false
}

// WritePostureHint is where provider writes are held now.
const WritePostureHint = "each merchant's write posture decides its provider writes: hold them with `openrails merchant hold --merchant NAME --mode readonly|limited --by NAME`, restore them with `openrails merchant arm`"

// retiredDBEnv are the db section's environment variables.
var retiredDBEnv = []string{"DB_URL", "DB_HOST", "DB_PORT", "DB_DATABASE", "DB_USERNAME", "DB_PASSWORD", "DB_SSLMODE", "DB_SSLROOTCERT", "DB_SQL_TRACE", "DB_LEASE_CONNS"}

// retiredSettings are former settings: a key, its environment variable (a
// section's ends in "_*") and what replaced it.
type retiredSetting struct{ key, env, now string }

var retiredSettings = []retiredSetting{
	{"provider_write_mode", "PROVIDER_WRITE_MODE", WritePostureHint},
	{"drain_delay", "DRAIN_DELAY", "on SIGTERM the server drains for 5s, then stops within 20s"},
	{"shutdown_timeout", "SHUTDOWN_TIMEOUT", "on SIGTERM the server drains for 5s, then stops within 20s"},
	{"usage_billing_enabled", "USAGE_BILLING_ENABLED", "a merchant with usage_billing terms bills its usage; one without has none"},
	{"private_port", "PRIVATE_PORT", "set metrics_port (METRICS_PORT), the operator's Prometheus /metrics listener"},
	{"cloudflare_proxies", "CLOUDFLARE_PROXIES", "Cloudflare's published ranges are built in; list only your own proxies in trusted_proxies"},
	{"rate_limits_disabled", "RATE_LIMITS_DISABLED", "set each rate_limits bucket's requests_per_minute to 0 to leave it unlimited; captcha is on only with its keys"},
	{"merchant_manifest_overlays", "MERCHANT_MANIFEST_OVERLAYS", "the merchant manifest is one file: keep PSP secrets in it and mount it from a Secret, or in Vault (vault.kv_mount)"},
	{"webhook_secret_overlap", "WEBHOOK_SECRET_OVERLAP", "a rotated-out webhook signing secret keeps verifying for 24 hours; retire_webhook_overlap ends it early"},
	{"return_origins", "RETURN_ORIGINS", "return URLs may name the billing origin, the merchant's issuer and its checkout.embed_origins"},
	{"event_retention", "EVENT_RETENTION", "the event log keeps 90 days"},
	{"llm.provider", "LLM_PROVIDER", noCatalogAssistant},
	{"llm.model", "LLM_MODEL", noCatalogAssistant},
	{"llm.base_url", "LLM_BASE_URL", noCatalogAssistant},
	{"llm.api_key", "LLM_API_KEY", noCatalogAssistant},
	{"llm.catalog_copilot_enabled", "LLM_CATALOG_COPILOT_ENABLED", noCatalogAssistant},
	{"llm.catalog_drafting_enabled", "LLM_CATALOG_DRAFTING_ENABLED", noCatalogAssistant},
	{"provider_sandbox", "PROVIDER_SANDBOX_*", "no setting redirects a provider: a PSP's environment (test or sandbox) and its test credentials reach the provider's own test system; tests wire fakes in Go (Deps.ProviderEndpoints)"},
}

// names reports whether an environment variable or secret file is the
// setting's.
func (r retiredSetting) names(name string) bool {
	if prefix, section := strings.CutSuffix(r.env, "*"); section {
		return strings.HasPrefix(name, prefix)
	}
	return name == r.env
}

func (r retiredSetting) inEnv() bool {
	if prefix, section := strings.CutSuffix(r.env, "*"); section {
		return hasEnvPrefix(prefix)
	}
	_, present := os.LookupEnv(r.env)
	return present
}

const noCatalogAssistant = "the catalog assistant was removed; OpenRails calls no language model"

// retiredAuthKeys are the server's former auth keys and their replacements.
var retiredAuthKeys = map[string]string{
	"auth.issuer":                        "auth.token.issuer (AUTH_TOKEN_ISSUER)",
	"auth.schema":                        "auth.database.schema (AUTH_DATABASE_SCHEMA)",
	"auth.keys_path":                     "auth.keys.path (AUTH_KEYS_PATH)",
	"auth.mint_disabled":                 "auth.keys.verify_only (AUTH_KEYS_VERIFY_ONLY)",
	"auth.allow_ephemeral_signing_key":   "auth.keys.allow_ephemeral_dev_keys (AUTH_KEYS_ALLOW_EPHEMERAL_DEV_KEYS)",
	"auth.allow_private_network_jwks":    "auth.token.allow_private_network_jwks (AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS)",
	"auth.allow_missing_senders":         "auth.registration.allow_missing_senders (AUTH_REGISTRATION_ALLOW_MISSING_SENDERS)",
	"auth.direct_peer_ip":                "auth.http.direct_peer_ip (AUTH_HTTP_DIRECT_PEER_IP)",
	"auth.request_origin":                "auth.resource.public_url (AUTH_RESOURCE_PUBLIC_URL)",
	"auth.naming":                        "naming (NAMING_*) for merchant names, auth.username for usernames",
	"auth.allow_loopback_http":           "nothing: AuthKit admits http only on a loopback host",
	"auth.allow_memory":                  "nothing: AuthKit keeps its limits and proofs in Redis or memory",
	"auth.active_key_id":                 "signing_key.active_key_id (AUTHKIT_ACTIVE_KEY_ID)",
	"auth.active_private_key_pem":        "signing_key.active_private_key_pem (AUTHKIT_ACTIVE_PRIVATE_KEY_PEM)",
	"auth.public_keys":                   "signing_key.public_keys (AUTHKIT_PUBLIC_KEYS)",
	"auth.default_tenant_id":             "nothing: AuthKit creates the server's one account directory, or adopts the one its database holds",
	"auth.keys.allow_ephemeral_dev_keys": "nothing: a loopback issuer's development keys are generated, under auth.keys.path when set",
	"resource_server":                    "auth.resource (id, public_url) and each merchant's trusted issuers in AuthKit",
}
