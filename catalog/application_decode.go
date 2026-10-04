package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/open-rails/openrails/internal/configdocument"
)

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
			if !ok {
				return fmt.Errorf("unknown field %q in catalog (schema names are case-sensitive)", name)
			}
			if err := applicationJSONNames(value, field); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		for _, value := range object {
			if err := applicationJSONNames(value, shape.Elem()); err != nil {
				return err
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
