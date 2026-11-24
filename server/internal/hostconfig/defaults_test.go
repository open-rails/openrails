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

// flatten is every key with a value in v, by its koanf path.
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
		name, opts, _ := strings.Cut(v.Type().Field(i).Tag.Get("koanf"), ",")
		path := prefix
		switch {
		case opts == "squash":
		case name == "" || name == "-":
			name = snake(v.Type().Field(i).Name)
			fallthrough
		default:
			path = strings.TrimPrefix(prefix+"."+name, ".")
		}
		if v.Field(i).Kind() == reflect.Map {
			continue // rate_limits: their own table
		}
		flatten(path, v.Field(i), out)
	}
}

// snake is an untagged field's key: matchField reads it without underscores.
func snake(field string) string {
	var b strings.Builder
	for i, r := range field {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
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
	flatten("", reflect.ValueOf(defaults()), want)
	f := defaults()
	cfg, err := f.config()
	if err != nil {
		t.Fatal(err)
	}
	want["public_billing_base_url"] = cfg.PublicBillingBaseURL
	want["db.url"] = defaultDatabaseURL
	want["naming.former_names.duration"] = f.Naming.FormerNames.Duration.String()
	want["auth.token.issuer"] = "public_billing_base_url"
	want["auth.default_tenant_id"] = cfg.Auth.DefaultTenantID.String()
	want["auth.token.refresh_token_duration"] = cfg.Auth.Token.RefreshTokenDuration.String()
	want["auth.database.schema"] = cfg.Auth.Database.Schema
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

// With nothing configured, the server runs on the defaults against Compose's
// dependencies: the database, its own URL, and a loopback issuer whose
// account directory is the local one.
func TestAnEmptyConfigurationLoadsTheDefaults(t *testing.T) {
	configPath, _ := isolate(t)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for got, want := range map[string]string{
		cfg.DB.URL:                        defaultDatabaseURL,
		cfg.PublicBillingBaseURL:          "http://localhost:3053",
		cfg.Auth.Token.Issuer:             "http://localhost:3053",
		cfg.Auth.DefaultTenantID.String(): localTenantID.String(),
		cfg.Database.Schema:               "billing",
		fmt.Sprint(cfg.Port):              "3053",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if cfg.Redis != nil || cfg.ClickHouse != nil || cfg.Vault != nil {
		t.Error("no Redis, ClickHouse or Vault is declared by default")
	}
}

// A deployed issuer names its own account directory: the local one never
// stands in for it, so boot refuses.
func TestOnlyALoopbackIssuerTakesTheLocalTenant(t *testing.T) {
	configPath, _ := isolate(t)
	t.Setenv("PUBLIC_BILLING_BASE_URL", "https://billing.example.com")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Token.Issuer != "https://billing.example.com" || !cfg.Auth.DefaultTenantID.IsZero() {
		t.Fatalf("issuer %q, tenant %s", cfg.Auth.Token.Issuer, cfg.Auth.DefaultTenantID)
	}
}
