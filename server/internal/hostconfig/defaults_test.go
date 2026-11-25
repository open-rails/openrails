package hostconfig

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// flatten is every key with a value in v, by its yaml path.
func flatten(prefix string, v reflect.Value, out map[string]string) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		if !v.IsZero() {
			out[prefix] = fmt.Sprint(v.Interface())
		}
		return
	}
	for i := 0; i < v.NumField(); i++ {
		name, opts, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
		path := prefix
		switch {
		case opts == "inline":
		case name == "" || name == "-":
			continue
		default:
			path = strings.TrimPrefix(prefix+"."+name, ".")
		}
		if v.Field(i).Kind() == reflect.Map {
			continue // maps: their own tables
		}
		flatten(path, v.Field(i), out)
	}
}

var (
	docRow    = regexp.MustCompile("^\\| (`[^|]+`) \\| ([^|]+) \\|")
	backticks = regexp.MustCompile("`([^`]+)`")
)

// standaloneDefaults are the Default column of docs/standalone.md's
// configuration reference, by key.
func standaloneDefaults(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/standalone.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(raw)
	section = section[strings.Index(section, "## Configuration reference"):]
	section = section[:strings.Index(section[3:], "\n## ")+3]
	out := map[string][]string{}
	for _, line := range strings.Split(section, "\n") {
		m := docRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var values []string
		for _, v := range backticks.FindAllStringSubmatch(m[2], -1) {
			values = append(values, v[1])
		}
		for _, k := range backticks.FindAllStringSubmatch(m[1], -1) {
			out[k[1]] = values
		}
	}
	return out
}

func sameValue(want, doc string) bool {
	if want == doc {
		return true
	}
	a, errA := time.ParseDuration(want)
	b, errB := time.ParseDuration(doc)
	return errA == nil && errB == nil && a == b
}

// Every default the loader gives, its own and its rules', is the one the
// configuration reference states.
func TestTheReferenceStatesEveryDefault(t *testing.T) {
	want := map[string]string{}
	d := defaults()
	flatten("", reflect.ValueOf(d.Server), want)
	flatten("", reflect.ValueOf(d), want)
	configPath, _ := isolate(t)
	t.Setenv("PUBLIC_URL", "https://billing.example.com")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want["database.url"] = defaultDatabaseURL
	want["auth.token.issuer"] = "public_url"
	want["auth.token.refresh_token_duration"] = cfg.Server.Auth.Token.RefreshTokenDuration.String()
	want["auth.database.schema"] = cfg.Server.Auth.Database.Schema
	documented := standaloneDefaults(t)
	for key, value := range want {
		values, ok := documented[key]
		if !ok {
			t.Errorf("%s (default %s) is not in the configuration reference with its default", key, value)
			continue
		}
		found := false
		for _, doc := range values {
			found = found || sameValue(value, doc)
		}
		if !found {
			t.Errorf("%s defaults to %s; the reference says %v", key, value, values)
		}
	}
}

// With nothing configured but public_url, the server runs on the defaults
// against Compose's dependencies: the database and a loopback issuer, whose
// account directory and development keys AuthKit makes. Without public_url
// boot refuses.
func TestAnEmptyConfigurationLoadsTheDefaults(t *testing.T) {
	configPath, _ := isolate(t)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for got, want := range map[string]string{
		cfg.Database.DSN(func(string) string { return "" }): defaultDatabaseURL,
		cfg.Server.Engine.PublicURL:                         "http://localhost:3053",
		cfg.Server.Auth.Token.Issuer:                        "http://localhost:3053",
		cfg.Server.Engine.Database.Schema:                   "billing",
		fmt.Sprint(cfg.Server.Port):                         "3053",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if cfg.Redis != nil || cfg.ClickHouse != nil || cfg.Vault != nil {
		t.Error("no Redis, ClickHouse or Vault is declared by default")
	}
	t.Setenv("PUBLIC_URL", "")
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "public_url (PUBLIC_URL) is required") {
		t.Errorf("no public_url: %v", err)
	}
}
