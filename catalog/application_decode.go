package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/open-rails/openrails/internal/configdocument"
)

// ReadFile reads a catalog file: YAML (.yaml, .yml) or JSON (.json). Pass the
// result as Config.Catalog. A catalog held in memory (go:embed) uses
// ParseApplicationYAML or ParseApplicationJSON instead.
func ReadFile(path string) (*Application, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var app *Application
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		app, err = ParseApplicationYAML(raw)
	case ".json":
		app, err = ParseApplicationJSON(raw)
	default:
		return nil, fmt.Errorf("catalog %s: use a .yaml, .yml or .json file", path)
	}
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	return app, nil
}

// ParseApplicationYAML rejects executable/reference-like YAML constructs and
// converts losslessly to the same bounded typed JSON contract used by HTTP.
func ParseApplicationYAML(raw []byte) (*Application, error) {
	body, err := configdocument.YAMLToJSON(raw, MaxApplicationBytes)
	if err != nil {
		return nil, err
	}
	return ParseApplicationJSON(body)
}

func ParseApplicationJSON(raw []byte) (*Application, error) {
	if err := configdocument.GuardJSON(raw, MaxApplicationBytes); err != nil {
		return nil, err
	}
	if err := applicationJSONNames(raw, reflect.TypeFor[Application]()); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out Application
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode catalog application: %w", err)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// keyedCollections are the records a catalog declares in maps keyed by key.
var keyedCollections = map[reflect.Type]string{
	reflect.TypeFor[ApplyProduct](): "product",
	reflect.TypeFor[ApplyPrice]():   "price",
	reflect.TypeFor[ApplyMeter]():   "meter",
}

// encoding/json accepts case-insensitive aliases for struct fields. Catalog
// declarations deliberately require exact schema names, while opaque map keys
// (entitlement names, provider names, dimensions) retain their original case.
func applicationJSONNames(raw []byte, shape reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if _, ok := reflect.Zero(shape).Interface().(interface{ validateField() error }); ok {
		value, _ := shape.FieldByName("Value")
		return applicationJSONNames(raw, value.Type)
	}
	switch shape.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		fields := map[string]reflect.Type{}
		if shape == reflect.TypeFor[ApplyPrice]() {
			for _, name := range []string{"amount", "access_duration", "billing_interval", "trial_duration"} {
				fields[name] = reflect.TypeFor[Field[string]]()
			}
		}
		for i := 0; i < shape.NumField(); i++ {
			field := shape.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		for name, value := range object {
			field, ok := fields[name]
			if noun, keyed := keyedCollections[shape]; !ok && keyed && name == "key" {
				return fmt.Errorf("field \"key\" is not allowed: a %s's map key is its key", noun)
			}
			if !ok {
				return fmt.Errorf("unknown field %q in catalog (schema names are case-sensitive)", name)
			}
			if err := applicationJSONNames(value, field); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case reflect.Map:
		if noun, keyed := keyedCollections[shape.Elem()]; keyed && bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return fmt.Errorf("must be a map keyed by %s key, not a list: catalog products, prices and meters are maps keyed by their key (write `<key>:`, not `- key: <key>`)", noun)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		for key, value := range object {
			if err := applicationJSONNames(value, shape.Elem()); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := applicationJSONNames(value, shape.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
