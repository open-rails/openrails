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
// HashiCorp's VAULT_ADDR and VAULT_CACERT, and AUTH_KEYS_PATH.
var envNames = map[string]string{
	"vault_addr":     "vault.address",
	"vault_cacert":   "vault.ca_cert",
	"auth_keys_path": "auth.keys.path",
}

// FileOnlySections are policy, set in config.yaml alone: the environment
// holds secrets and per-environment facts. A variable naming one refuses
// boot.
var FileOnlySections = []string{"rate_limits"}

// fileOnlySection is the file-only section key is in, "" for none.
func fileOnlySection(key string) string {
	for _, section := range FileOnlySections {
		if key == section || strings.HasPrefix(key, section+".") {
			return section
		}
	}
	return ""
}

// envKeyToConfigKey maps an environment variable's name to its key:
// PUBLIC_URL is public_url, DATABASE_URL
// database.url. A name that routes to no section is "" and never loaded (the
// process environment is full of names that are not ours: PATH, HOME,
// Compose's interpolation); a name inside a section that hits no key
// (DATABASE_TYPO) refuses boot like any unknown key.
func envKeyToConfigKey(s string) string {
	s = strings.ToLower(s)

	if key, ok := envNames[s]; ok {
		return key
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
		if section := fileOnlySection(mapped); section != "" {
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
	// A clickhouse.url any source names, empty included, is the operator's.
	if cfg.ClickHouse != nil && k.Exists("clickhouse.url") {
		cfg.ClickHouse.Default = false
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
