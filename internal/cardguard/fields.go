package cardguard

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

var (
	cardType = reflect.TypeFor[Card]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// PANField is the JSON path of the first field of v, a decoded request, that
// holds a card number; "" when none does. Every field is read but a Card, the
// one place a card number belongs; a json.RawMessage is read as the JSON it
// holds. The path never contains the number.
func PANField(v any) string {
	return panField(reflect.ValueOf(v), "")
}

func panField(v reflect.Value, path string) string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return panField(v.Elem(), path)
		}
	case reflect.String:
		if ContainsPAN(v.String()) {
			return path
		}
	case reflect.Struct:
		if v.Type().ConvertibleTo(cardType) {
			return ""
		}
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" || !f.IsExported() && !f.Anonymous {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			at := join(path, name, f.Name)
			if f.Anonymous && name == "" {
				at = path // its fields are the struct's own on the wire
			}
			if found := panField(v.Field(i), at); found != "" {
				return found
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type() == rawType {
			var decoded any
			if json.Unmarshal(v.Bytes(), &decoded) != nil {
				return panField(reflect.ValueOf(string(v.Bytes())), path)
			}
			return panField(reflect.ValueOf(decoded), path)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return ""
		}
		for i := range v.Len() {
			if found := panField(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, key := range keys {
			name := fmt.Sprint(key)
			if ContainsPAN(name) {
				return path
			}
			if found := panField(v.MapIndex(key), join(path, name, name)); found != "" {
				return found
			}
		}
	}
	return ""
}

// join is path's child named name, or fallback without one.
func join(path, name, fallback string) string {
	if name == "" {
		name = fallback
	}
	if path == "" {
		return name
	}
	return path + "." + name
}
