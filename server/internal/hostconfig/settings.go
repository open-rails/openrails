package hostconfig

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/config"
)

// Setting is one environment variable the binary reads.
type Setting struct {
	// Path is the key it sets (database.url); empty for a variable that is
	// no key.
	Path string
	// Env is the variable (DATABASE_URL).
	Env string
	// Default is its value with nothing configured, as the variable spells it.
	Default string
	// Owner and Field are the struct field the key decodes into.
	Owner reflect.Type
	Field string
	// Doc describes a variable that is no key.
	Doc string
}

// Settings are every variable the binary reads, in file order: the
// process's own, then each key of server.Config and of Config.
func Settings() ([]Setting, error) {
	finished := defaults()
	if err := finished.finish(config.DefaultSecretsPath); err != nil {
		return nil, err
	}
	raw := defaults()
	raw.Database.URL = finished.Database.URL
	out := []Setting{
		{Env: "OPENRAILS_CONFIG", Default: "config.yaml", Doc: "The configuration file when --config names none; also config/ is searched."},
		{Env: "VAULT_SECRETS_PATH", Default: config.DefaultSecretsPath, Doc: "The directory of mounted secret files, each named like its variable; vault.secrets_path defaults to it."},
	}
	var err error
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := 0; i < t.NumField() && err == nil; i++ {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			fv := v.Field(i)
			if opts == "inline" {
				walk(fv, prefix)
				continue
			}
			if name == "" || name == "-" || !f.IsExported() {
				continue
			}
			path := strings.TrimPrefix(prefix+"."+name, ".")
			if _, retired := retiredAuthKeys[path]; retired || fileOnlySection(path) != "" {
				continue
			}
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					fv = reflect.Zero(fv.Type().Elem())
				} else {
					fv = fv.Elem()
				}
			}
			if fv.Kind() == reflect.Struct && !leaf(fv.Type()) {
				walk(fv, path)
				continue
			}
			env := envName(path)
			if env == "VAULT_SECRETS_PATH" { // the secrets directory's
				continue
			}
			if back := envKeyToConfigKey(env); back != path {
				err = fmt.Errorf("%s sets %q, not %s", env, back, path)
				return
			}
			value, verr := envValue(fv)
			if verr != nil {
				err = fmt.Errorf("%s: %w", path, verr)
				return
			}
			out = append(out, Setting{Path: path, Env: env, Default: value, Owner: t, Field: f.Name})
		}
	}
	walk(reflect.ValueOf(finished.Server), "")
	walk(reflect.ValueOf(*raw), "")
	return out, err
}

// envName is the variable that sets key.
func envName(key string) string {
	for env, k := range envNames {
		if k == key {
			return strings.ToUpper(env)
		}
	}
	return strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

var (
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	durationType  = reflect.TypeFor[time.Duration]()
)

// leaf reports a struct the environment sets as one value.
func leaf(t reflect.Type) bool {
	return t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler)
}

// envValue is v as its variable spells it: a scalar as is, a list of scalars
// comma-separated, anything else as JSON by its yaml keys.
func envValue(v reflect.Value) (string, error) {
	if v.Kind() == reflect.Bool {
		return strconv.FormatBool(v.Bool()), nil
	}
	if v.IsZero() {
		return "", nil
	}
	if v.Type() == durationType {
		return shortDuration(time.Duration(v.Int())), nil
	}
	if m, ok := v.Interface().(encoding.TextMarshaler); ok {
		text, err := m.MarshalText()
		return string(text), err
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), nil
	case reflect.Slice:
		if k := v.Type().Elem().Kind(); k != reflect.Struct && k != reflect.Map && k != reflect.Pointer {
			parts := make([]string, v.Len())
			for i := range parts {
				s, err := envValue(v.Index(i))
				if err != nil {
					return "", err
				}
				parts[i] = s
			}
			return strings.Join(parts, ","), nil
		}
	}
	raw, err := json.Marshal(byYAMLKeys(v))
	return string(raw), err
}

// byYAMLKeys is v with each struct as a map of its yaml keys.
func byYAMLKeys(v reflect.Value) any {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == durationType || leaf(v.Type()) {
			return v.Interface()
		}
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
			if name != "" && name != "-" && !v.Field(i).IsZero() {
				out[name] = byYAMLKeys(v.Field(i))
			}
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		for _, k := range v.MapKeys() {
			out[fmt.Sprint(k.Interface())] = byYAMLKeys(v.MapIndex(k))
		}
		return out
	case reflect.Slice:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = byYAMLKeys(v.Index(i))
		}
		return out
	}
	if v.Type() == durationType {
		return shortDuration(time.Duration(v.Int()))
	}
	return v.Interface()
}

// shortDuration is d as a config file writes it: 2160h, 90s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
